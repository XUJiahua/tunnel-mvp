package diagnostics

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompactErrorSurvivesRemotedialer100ByteLimit(t *testing.T) {
	f := New("connection_refused", "tcp", strings.Repeat("目标连接被拒绝", 100))
	f.AgentRequestID = ID()
	encoded := f.Error()
	if len(encoded) > 100 || !utf8.ValidString(encoded) {
		t.Fatal("error frame exceeds safe limit")
	}
	decoded := Describe(errors.New("HTTP transport failed: " + encoded))
	if decoded.Code != f.Code || decoded.Stage != f.Stage || decoded.AgentRequestID != f.AgentRequestID {
		t.Fatalf("correlation lost: %+v", decoded)
	}
	// Full in-process diagnosis must not lose details to the compact wire format.
	if Describe(f).Message != f.Message {
		t.Fatal("local diagnosis was truncated")
	}
}
func TestLegacyAgentErrorClassification(t *testing.T) {
	f := Describe(errors.New("target denied by agent ACL"))
	if f.Code != "acl_denied" || f.Hint == "" {
		t.Fatalf("legacy failure lost: %+v", f)
	}
}
