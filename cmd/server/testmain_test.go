package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := os.Setenv("ALLOW_INSECURE_MAPPING_URL", "1"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
