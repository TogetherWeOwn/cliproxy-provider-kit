package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripEchoNamespace(t *testing.T) {
	out := stripEchoNamespace([]byte(`{"model":"echo/echo-model","messages":[]}`))
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil || body["model"] != "echo-model" {
		t.Fatalf("not stripped: %s", out)
	}
	if got := string(stripEchoNamespace([]byte(`{"model":"echo-model"}`))); !strings.Contains(got, `"echo-model"`) {
		t.Fatalf("bare id changed: %s", got)
	}
}

func TestStripEchoNamespaceKeepsLargeIntegers(t *testing.T) {
	in := `{"model":"echo/echo-model","seed":9007199254740993,"nested":{"count":9007199254740993}}`
	out := stripEchoNamespace([]byte(in))
	if !strings.Contains(string(out), `"seed":9007199254740993`) || !strings.Contains(string(out), `"count":9007199254740993`) {
		t.Fatalf("numbers rounded: %s", out)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil || body["model"] != "echo-model" {
		t.Fatalf("model not stripped: %s", out)
	}
}
