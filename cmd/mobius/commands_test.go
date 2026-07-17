package main

import "testing"

func TestParseInt64ArgRejectsTrailingGarbage(t *testing.T) {
	for _, value := range []string{"12oops", "12 oops", "1.5", "", "0", "-1"} {
		if _, err := parseInt64Arg(value, "secret-version"); err == nil {
			t.Fatalf("parseInt64Arg(%q) succeeded", value)
		}
	}
	if got, err := parseInt64Arg("12", "secret-version"); err != nil || got != 12 {
		t.Fatalf("parseInt64Arg valid value = %d, %v", got, err)
	}
}
