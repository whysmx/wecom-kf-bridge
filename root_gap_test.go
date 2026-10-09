package bridge

import (
	"net/http"
	"testing"
)

func TestRootCoverageGapBranches(t *testing.T) {
	// Method validation happens before token lookup on the send endpoint.
	s := testServer(MemoryAdapter{})
	if rr := req(s, http.MethodGet, "/cgi-bin/message/send", ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("send method status %d", rr.Code)
	}

	// A callback request with no configured bindings must still terminate with
	// the protocol's authentication error rather than panic or succeed.
	empty := NewServer(Config{})
	if rr := req(empty, http.MethodGet, "/wecom/callback?msg_signature=x&timestamp=1&nonce=n&echostr=x", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("empty callback status %d", rr.Code)
	}

	if _, err := ChunkUTF8("abc", 0); err != nil {
		t.Fatal(err)
	}
	if validAgentID("") {
		t.Fatal("empty agent id accepted")
	}
}
