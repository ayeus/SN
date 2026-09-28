package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestRazorpayWebhookSignature(t *testing.T) {
	rz := newRazorpay("rzp_test_key", "secret", "whsec")
	body := []byte(`{"event":"order.paid"}`)
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write(body)
	good := hex.EncodeToString(mac.Sum(nil))

	if !rz.verifyWebhook(body, good) {
		t.Fatal("valid signature rejected")
	}
	flip := "0"
	if good[len(good)-1] == '0' {
		flip = "1"
	}
	if rz.verifyWebhook(body, good[:len(good)-1]+flip) {
		t.Fatal("tampered signature accepted")
	}
	if rz.verifyWebhook([]byte(`{"event":"order.paid","x":1}`), good) {
		t.Fatal("signature for a different body accepted")
	}
}

func TestRazorpayDisabledWithoutAllSecrets(t *testing.T) {
	if newRazorpay("id", "secret", "") != nil {
		t.Fatal("a missing webhook secret must disable Razorpay, not accept unsigned webhooks")
	}
	var rz *Razorpay
	if rz.keyIDOrEmpty() != "" {
		t.Fatal("nil client should report no key id")
	}
}

func TestDecimalRatio(t *testing.T) {
	num, den, err := decimalRatio("0.1800")
	if err != nil || num*100/den != 18 {
		t.Fatalf("0.18 → %d/%d %v", num, den, err)
	}
}
