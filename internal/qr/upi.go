package qr

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// GenerateUPILink builds a static personal-VPA UPI intent URL.
// am is rupees (paise/100); tn is always PG-<due_code>. Room is appended to pn when set.
func GenerateUPILink(vpa, ownerName string, amountPaise int64, dueCode, room string) string {
	rupees := float64(amountPaise) / 100.0
	pn := strings.TrimSpace(ownerName)
	if r := strings.TrimSpace(room); r != "" {
		if pn == "" {
			pn = "Room " + r
		} else {
			pn = pn + " (Room " + r + ")"
		}
	}
	q := url.Values{}
	q.Set("pa", strings.TrimSpace(vpa))
	q.Set("pn", pn)
	q.Set("am", fmt.Sprintf("%.2f", rupees))
	q.Set("cu", "INR")
	q.Set("tn", domain.UPINote(dueCode))
	return "upi://pay?" + q.Encode()
}
