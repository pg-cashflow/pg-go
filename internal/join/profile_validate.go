package join

import (
	"strings"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrInvalidOccupationType = ErrProfileIncomplete // mapped at HTTP layer if needed
	ErrEmergencySameAsLogin  = ErrProfileIncomplete
	ErrMinorGuardianRequired = ErrProfileIncomplete
)

// ValidateProfileV1 checks mandatory fields for profile_schema_version 1.
func ValidateProfileV1(in ProfileInput, loginPhone string, schemaVersion int) error {
	if schemaVersion != 1 {
		schemaVersion = 1
	}
	phone := strings.TrimSpace(loginPhone)
	emergency := strings.TrimSpace(in.EmergencyPhone)
	if emergency == "" || strings.TrimSpace(in.EmergencyContactName) == "" {
		return ErrProfileIncomplete
	}
	if phone != "" && !strings.HasPrefix(phone, "+G") && emergency == phone {
		return ErrEmergencySameAsLogin
	}
	switch in.OccupationType {
	case domain.OccupationStudent:
		if strings.TrimSpace(in.Details.CollegeName) == "" || strings.TrimSpace(in.Details.Course) == "" || strings.TrimSpace(in.Details.Year) == "" {
			return ErrProfileIncomplete
		}
		if strings.TrimSpace(in.Details.ParentGuardianName) == "" || strings.TrimSpace(in.Details.ParentGuardianPhone) == "" {
			return ErrProfileIncomplete
		}
	case domain.OccupationWorkingProfessional:
		if strings.TrimSpace(in.Details.EmployerName) == "" {
			return ErrProfileIncomplete
		}
	case domain.OccupationOther:
	default:
		return ErrProfileIncomplete
	}
	if in.DateOfBirth != nil {
		age := ageYears(*in.DateOfBirth, time.Now().UTC())
		if age < 18 {
			if strings.TrimSpace(in.GuardianName) == "" || strings.TrimSpace(in.GuardianPhone) == "" {
				return ErrMinorGuardianRequired
			}
		}
	}
	return nil
}

func ageYears(dob time.Time, now time.Time) int {
	y := now.Year() - dob.Year()
	if now.Month() < dob.Month() || (now.Month() == dob.Month() && now.Day() < dob.Day()) {
		y--
	}
	return y
}
