package channel

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	sec := json.RawMessage(`{"client_id":"xyz","client_secret":"shpat_abcdef123","nested":{"token":"tok-999999"},"list":["listsecret1"]}`)
	got := Redact("bad token shpat_abcdef123 for client xyz; tok-999999 listsecret1", sec)
	if got != "bad token *** for client xyz; *** ***" {
		t.Fatalf("Redact = %q", got)
	}
	if Redact("abc", json.RawMessage(`not json`)) != "abc" {
		t.Fatal("secrets 解不开时应原样返回")
	}
	// 一个值是另一个的子串：长的先换，不留残片。
	if got := Redact("x secret123456789 y", json.RawMessage(`{"a":"secret123","b":"secret123456789"}`)); got != "x *** y" {
		t.Fatalf("子串情形 Redact = %q", got)
	}
}

func TestRedactErrorKeepsIdentity(t *testing.T) {
	sec := json.RawMessage(`{"client_secret":"secret123456"}`)
	err := RedactError(&RetryableError{RateLimited: true, Err: errors.New("boom secret123456")}, sec)
	if strings.Contains(err.Error(), "secret123456") {
		t.Fatalf("脱敏后仍含凭据：%q", err.Error())
	}
	var re *RetryableError
	if !errors.As(err, &re) || !re.RateLimited {
		t.Fatal("脱敏包装挡住了 errors.As")
	}
	cred := RedactError(errors.Join(ErrCredentials, errors.New("secret123456")), sec)
	if !errors.Is(cred, ErrCredentials) || strings.Contains(cred.Error(), "secret123456") {
		t.Fatalf("ErrCredentials 穿透或脱敏失败：%q", cred.Error())
	}
	if RedactError(nil, sec) != nil {
		t.Fatal("nil 应返回 nil")
	}
	plain := errors.New("nothing to hide")
	if RedactError(plain, sec) != plain {
		t.Fatal("无需脱敏时应原样返回")
	}
}
