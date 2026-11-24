package main

import "testing"

func TestSandboxTopUpsOnlyInDev(t *testing.T) {
	for env, want := range map[string]bool{"dev": true, "": false, "production": false, "staging": false, "Dev": false} {
		got := sandboxTopUps(func(k string) string {
			if k == "APP_ENV" {
				return env
			}
			return ""
		})
		if got != want {
			t.Errorf("APP_ENV=%q: sandbox %v", env, got)
		}
	}
}
