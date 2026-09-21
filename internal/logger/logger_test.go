package logger

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDisabledLogsNothing(t *testing.T) {
	var buf bytes.Buffer
	l := New(Config{Enabled: false, Out: &buf})
	l.Infof("hello %s", "world")
	l.Debug("debug message")
	l.Error("boom")
	if buf.Len() != 0 {
		t.Fatalf("expected no output from disabled logger, got %q", buf.String())
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	l := New(Config{Enabled: true, Level: "error", Out: &buf})
	l.Infof("should be filtered")
	l.Errorf("should appear")
	if strings.Contains(buf.String(), "should be filtered") {
		t.Fatalf("info line leaked through error-level logger: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "should appear") {
		t.Fatalf("error line missing: %q", buf.String())
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(Config{Enabled: true, Level: "info", Format: "json", Out: &buf})
	l.Info("request", "status", 200, "id", "abc12345")

	var rec map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("invalid JSON log line: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "request" {
		t.Fatalf("msg missing: %v", rec)
	}
	if rec["status"] != float64(200) || rec["id"] != "abc12345" {
		t.Fatalf("attrs missing: %v", rec)
	}
}

func TestWithAddsAttributes(t *testing.T) {
	var buf bytes.Buffer
	l := New(Config{Enabled: true, Format: "json", Out: &buf})
	l.With("source", "test").Info("done")
	var rec map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if rec["source"] != "test" {
		t.Fatalf("expected bound attr, got %v", rec)
	}
}

func TestNopIsSafe(t *testing.T) {
	n := Nop()
	n.Infof("ignored")
	n.Info("ignored", "a", 1)
	n.Printf("ignored")
}
