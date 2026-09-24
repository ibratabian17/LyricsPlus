package main

import (
	"strings"
	"testing"
)

func TestPromptFallback(t *testing.T) {
	ap := &app{
		reader: strings.NewReader("hello\n"),
	}
	res := ap.prompt("Test", "default")
	if res != "hello" {
		t.Fatalf("expected hello, got %q", res)
	}
}

func TestPromptLongInput(t *testing.T) {
	longStr := strings.Repeat("A", 5000)
	ap := &app{
		reader: strings.NewReader(longStr + "\n"),
	}
	res := ap.prompt("LongInput", "")
	if res != longStr {
		t.Fatalf("expected length %d, got %d", len(longStr), len(res))
	}
}

func TestPromptDefault(t *testing.T) {
	ap := &app{
		reader: strings.NewReader("\n"),
	}
	res := ap.prompt("Test", "fallback-val")
	if res != "fallback-val" {
		t.Fatalf("expected fallback-val, got %q", res)
	}
}
