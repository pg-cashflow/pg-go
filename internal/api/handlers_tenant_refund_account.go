package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/crypto"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ifscPattern        = regexp.MustCompile(`^[A-Z]{4}0[A-Z0-9]{6}$`)
	accountNumPattern  = regexp.MustCompile(`^[A-Za-z0-9]{6,34}$`)
)

type registerRefundAccountBody struct {
	AccountNumber string `json:"account_number" binding:"required"`
	IFSC          string `json:"ifsc" binding:"required"`
}

// TenantGetRefundAccount handles GET /tenant/refund-account.
// Returns the verified refund account for the authenticated tenant if available.
func (h *Handlers) TenantGetRefundAccount(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payout repository unavailable"})
		return
	}

	payee, err := repo.GetPayeeByTenant(c.Request.Context(), t.PropertyID, t.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusOK, gin.H{"has_account": false})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query refund account"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"has_account":          true,
		"payee_id":             payee.ID,
		"account_number_last4": payee.AccountNumberLast4,
		"ifsc":                 payee.IFSC,
		"bank_name":            payee.BankName,
		"name_at_bank":         payee.NameAtBank,
		"verification_status":  payee.VerificationStatus,
		"is_verified":          payee.IsVerified,
		"verified_at":          payee.VerifiedAt,
	})
}

// TenantRegisterRefundAccount handles POST /tenant/refund-account.
// Performs real-time penny-drop bank verification and Aadhaar legal name matching.
func (h *Handlers) TenantRegisterRefundAccount(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}

	var body registerRefundAccountBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "account_number and ifsc are required", apierr.CodeRequestInvalidBody)
		return
	}

	cleanAccount := strings.TrimSpace(body.AccountNumber)
	cleanIFSC := strings.ToUpper(strings.TrimSpace(body.IFSC))

	if !accountNumPattern.MatchString(cleanAccount) {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid bank account number format (must be 6-34 alphanumeric characters)", apierr.CodeRequestInvalidBody)
		return
	}
	if !ifscPattern.MatchString(cleanIFSC) {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid IFSC format (must be 11 characters, e.g. HDFC0001234)", apierr.CodeRequestInvalidBody)
		return
	}

	tenantLegalName := strings.TrimSpace(t.Name)
	if tenantLegalName == "" {
		apierr.RespondClientErr(c, http.StatusForbidden, "tenant legal name not registered", apierr.CodeAuthProfileIncomplete)
		return
	}

	var verifier BankAccountVerifier
	if h.BankVerifier != nil {
		verifier = h.BankVerifier
	} else if cf, ok := h.CashfreeClient.(BankAccountVerifier); ok {
		verifier = cf
	}

	if verifier == nil {
		apierr.RespondClientErr(c, http.StatusServiceUnavailable, "bank account verification service unavailable", apierr.CodeBankVerifierUnavailable)
		return
	}

	tenantPhone := ""
	if t.Phone != nil {
		tenantPhone = *t.Phone
	}

	verifyResult, err := verifier.VerifyBankAccountSync(c.Request.Context(), cashfree.BankAccountVerificationRequest{
		BankAccount: cleanAccount,
		IFSC:        cleanIFSC,
		Name:        tenantLegalName,
		Phone:       tenantPhone,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "verification provider call failed", "details": err.Error()})
		return
	}

	if !verifyResult.IsValid() {
		apierr.RespondClientErr(c, http.StatusUnprocessableEntity, "bank account is invalid or inactive at destination bank", apierr.CodeBankAccountInvalid)
		return
	}

	// Name match validation
	if !isAcceptableNameMatch(verifyResult.NameMatchResult, verifyResult.NameMatchScore) {
		apierr.RespondClientErr(c, http.StatusUnprocessableEntity, "bank account holder name does not match tenant registered legal name", apierr.CodeBankNameMismatch)
		return
	}

	// Secure encryption and hashing
	keyRing := h.getPayoutKeyRing()
	encAcct, err := crypto.EncryptWithKeyRing(keyRing, []byte(cleanAccount))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to secure bank account"})
		return
	}

	salt := []byte(h.getChecksumSecret())
	acctHash := domain.ComputeAccountHash(salt, cleanAccount)

	var last4 string
	if len(cleanAccount) >= 4 {
		last4 = cleanAccount[len(cleanAccount)-4:]
	} else {
		last4 = cleanAccount
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repository unavailable"})
		return
	}

	var scorePtr *float64
	if verifyResult.NameMatchScore != "" {
		if s, parseErr := strconv.ParseFloat(verifyResult.NameMatchScore, 64); parseErr == nil {
			scorePtr = &s
		}
	}

	bankName := verifyResult.BankName
	if bankName == "" {
		bankName = cleanIFSC[:4]
	}

	payee := &domain.PayoutPayee{
		PropertyID:             t.PropertyID,
		TenantID:               &t.ID,
		PayeeType:              domain.PayeeTypeTenantDeposit,
		Name:                   tenantLegalName,
		Phone:                  t.Phone,
		AccountNumberEncrypted: encAcct,
		AccountNumberLast4:     &last4,
		AccountNumberHash:      acctHash,
		IFSC:                   &cleanIFSC,
		BankName:               &bankName,
		KeyVersion:             1,
		IsVerified:             true,
		VerificationStatus:     domain.PayeeVerificationProviderVerified,
		VerificationMethod:     "cashfree_bank_sync",
		VerificationRef:        &verifyResult.ReferenceID,
		NameAtBank:             &verifyResult.NameAtBank,
		NameMatchScore:         scorePtr,
		NameMatchResult:        &verifyResult.NameMatchResult,
	}

	savedPayee, err := repo.SaveVerifiedTenantPayee(c.Request.Context(), payee)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save verified refund account"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"has_account":          true,
		"payee_id":             savedPayee.ID,
		"account_number_last4": savedPayee.AccountNumberLast4,
		"ifsc":                 savedPayee.IFSC,
		"bank_name":            savedPayee.BankName,
		"name_at_bank":         savedPayee.NameAtBank,
		"name_match_score":     savedPayee.NameMatchScore,
		"name_match_result":    savedPayee.NameMatchResult,
		"verification_status":  savedPayee.VerificationStatus,
		"is_verified":          savedPayee.IsVerified,
		"verified_at":          savedPayee.VerifiedAt,
	})
}

func isAcceptableNameMatch(matchResult, matchScore string) bool {
	cleanRes := strings.ToUpper(strings.TrimSpace(matchResult))
	if cleanRes == "DIRECT_MATCH" || cleanRes == "GOOD_PARTIAL_MATCH" {
		return true
	}
	if cleanRes == "POOR_PARTIAL_MATCH" || cleanRes == "NO_MATCH" {
		return false
	}
	if matchScore != "" {
		if s, err := strconv.ParseFloat(matchScore, 64); err == nil {
			return s >= 60.0
		}
	}
	return false
}
