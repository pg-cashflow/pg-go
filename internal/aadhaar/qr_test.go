package aadhaar

import "testing"

func TestDecodeAadhaarQRXML(t *testing.T) {
	raw := `<PrintLetterBarcodeData uid="123456789012" name="Ram Kumar" gender="M" dob="01-01-1990"/>`
	data, partial, err := DecodeAadhaarQR(raw)
	if err != nil {
		t.Fatal(err)
	}
	if partial {
		t.Fatalf("expected complete decode, got partial %+v", data)
	}
	if data.Name != "Ram Kumar" || data.UIDLast4 != "9012" || data.Gender != "M" || data.DOB != "01-01-1990" {
		t.Fatalf("data=%+v", data)
	}
}

func TestDecodeAadhaarQRPartialXML(t *testing.T) {
	raw := `name="Sita" gender="F" uid="999988887777"`
	data, partial, err := DecodeAadhaarQR(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !partial {
		t.Fatal("expected partial")
	}
	if data.Name != "Sita" || data.UIDLast4 != "7777" {
		t.Fatalf("data=%+v", data)
	}
}

func TestDecodeAadhaarQRUnreadable(t *testing.T) {
	data, partial, err := DecodeAadhaarQR("\x00\x01secure-qr-binary")
	if err != nil {
		t.Fatal(err)
	}
	if !partial {
		t.Fatal("expected partial for unreadable")
	}
	if data.Name != "" {
		t.Fatalf("expected blank name, got %q", data.Name)
	}
}

func TestDecodeAadhaarQREmpty(t *testing.T) {
	_, partial, err := DecodeAadhaarQR("")
	if err != nil || !partial {
		t.Fatalf("empty: partial=%v err=%v", partial, err)
	}
}
