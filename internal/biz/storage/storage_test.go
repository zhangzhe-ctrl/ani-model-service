package storage

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestVerifyReader(t *testing.T) {
	if err := VerifyReader(bytes.NewBufferString("model"), "9372c470eeadd5ecd9c3c74c2b3cb633f8e2f2fad799250a0f70d652b6b825e4"); err != nil {
		t.Fatalf("expected checksum match, got %v", err)
	}
	if err := VerifyReader(bytes.NewBufferString("model"), "0002c470eeadd5ecd9c3c74c2b3cb633f8e2f2fad799250a0f70d652b6b825e4"); err == nil || !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	// SHA256("model")
	if err := VerifyReader(bytes.NewBufferString("model"), "937cc19c7b4d3b4e5f2f8f4f6f0f6d9e4f3f5b2e3a1b4c5d6e7f8a9b0c1d2e3f"); err == nil {
		t.Fatal("expected mismatch for invalid digest")
	}
}

func TestVerifyReaderWithSize(t *testing.T) {
	got, err := VerifyReaderWithSize(bytes.NewBufferString("model"), int64(len("model")), "9372c470eeadd5ecd9c3c74c2b3cb633f8e2f2fad799250a0f70d652b6b825e4")
	if err != nil || !got.Matches || got.SizeBytes != 5 {
		t.Fatalf("verification=%+v err=%v", got, err)
	}
	got, err = VerifyReaderWithSize(bytes.NewBufferString("model"), 4, "9372c470eeadd5ecd9c3c74c2b3cb633f8e2f2fad799250a0f70d652b6b825e4")
	if err != nil || got.Matches || got.SizeBytes != 5 {
		t.Fatalf("size mismatch verification=%+v err=%v", got, err)
	}
}

func TestValidateSignedURL(t *testing.T) {
	now := time.Unix(100, 0)
	if err := ValidateSignedURL(SignedURL{URL: "https://storage/object", ExpiresAt: now.Add(time.Minute)}, now); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ValidateSignedURL(SignedURL{URL: "x", ExpiresAt: now}, now), ErrURLExpired) {
		t.Fatal("expected expiry")
	}
}
