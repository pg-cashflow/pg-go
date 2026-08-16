package mailer

import (
	"context"
	"strings"
	"testing"
)

func TestSMTPMailerImplementsMailer(t *testing.T) {
	var m Mailer = &SMTPMailer{}
	_ = m
}

func TestNoopMailerImplementsMailer(t *testing.T) {
	var m Mailer = NoopMailer{}
	if err := m.Send(context.Background(), "a@b.c", "subj", "<p>hi</p>"); err != nil {
		t.Fatalf("NoopMailer.Send: %v", err)
	}
}

func TestBuildMessage(t *testing.T) {
	msg := string(buildMessage("from@ex.com", "to@ex.com", "Hello", "<b>x</b>"))
	for _, want := range []string{
		"From: from@ex.com",
		"To: to@ex.com",
		"Subject: Hello",
		"Content-Type: text/html",
		"<b>x</b>",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestSMTPMailer_NotConfigured(t *testing.T) {
	m := &SMTPMailer{}
	if err := m.Send(context.Background(), "to@ex.com", "s", "b"); err == nil {
		t.Fatal("expected not configured error")
	}
}
