package main

import (
	"strings"
	"testing"
	"time"
)

func TestEnvKnobsKeepTheirDefaultsOnEmptyOrGarbage(t *testing.T) {
	t.Setenv("ECHO_TEST_STR", "")
	if got := getEnv("ECHO_TEST_STR", "d"); got != "d" {
		t.Errorf("empty string variable read as %q", got)
	}
	t.Setenv("ECHO_TEST_INT", "many")
	if got := getEnvInt("ECHO_TEST_INT", 3); got != 3 {
		t.Errorf("getEnvInt on garbage = %d", got)
	}
	t.Setenv("ECHO_TEST_INT", "250")
	if got := getEnvInt("ECHO_TEST_INT", 3); got != 250 {
		t.Errorf("getEnvInt = %d", got)
	}
	t.Setenv("ECHO_TEST_FLOAT", "half")
	if got := getEnvFloat("ECHO_TEST_FLOAT", 0.25); got != 0.25 {
		t.Errorf("getEnvFloat on garbage = %v", got)
	}
	t.Setenv("ECHO_TEST_FLOAT", "0.5")
	if got := getEnvFloat("ECHO_TEST_FLOAT", 0.25); got != 0.5 {
		t.Errorf("getEnvFloat = %v", got)
	}
}

func TestLoadConfigReadsEveryKnob(t *testing.T) {
	t.Setenv("ADVISOR_ADDR", "127.0.0.1:1234")
	t.Setenv("ECHO_MODE", "fixed")
	t.Setenv("ECHO_TEXT", "always this")
	t.Setenv("ECHO_DELAY_MS", "40")
	t.Setenv("ECHO_FAIL_RATE", "0.1")
	cfg := loadConfig()
	if cfg.addr != "127.0.0.1:1234" || cfg.mode != "fixed" || cfg.text != "always this" || cfg.delay != 40*time.Millisecond || cfg.failRate != 0.1 {
		t.Fatalf("loadConfig = %+v", cfg)
	}
}

func TestTokenEstimateNeverReportsZeroForText(t *testing.T) {
	cases := map[int]int{0: 0, 1: 1, 3: 1, 4: 1, 8: 2, 400: 100}
	for chars, want := range cases {
		if got := estimateTokensFromChars(chars); got != want {
			t.Errorf("estimateTokensFromChars(%d) = %d, want %d", chars, got, want)
		}
	}
	if got := estimateTokens("abcdefgh"); got != 2 {
		t.Errorf("estimateTokens of eight bytes = %d", got)
	}
}

func TestCompletionIDsLookLikeOpenAIsAndDoNotRepeat(t *testing.T) {
	a, b := randomID(), randomID()
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, "chatcmpl-") || len(id) != len("chatcmpl-")+24 {
			t.Errorf("id %q is not chatcmpl- plus 24 hex characters", id)
		}
	}
	if a == b {
		t.Fatal("two ids in a row were identical")
	}
}
