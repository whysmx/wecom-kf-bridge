package bridge

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestSafeFileName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":               "file",
		"report.pdf":     "report.pdf",
		"../dir/doc.pdf": "doc.pdf",
		`C:\dir\y.doc`:   "y.doc",
		".":              "file",
		"..":             "file",
		"/":              "file",
		"a/b/../c.txt":   "c.txt",
	}
	for in, want := range cases {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEncodeDecodeMediaPayload(t *testing.T) {
	t.Parallel()
	raw := EncodeMediaPayload("MID123", "../x/a.pdf")
	p, ok := DecodeMediaPayload(raw)
	if !ok || p.MediaID != "MID123" || p.FileName != "a.pdf" {
		t.Fatalf("roundtrip: %+v ok=%v raw=%s", p, ok, raw)
	}
	p2, ok := DecodeMediaPayload("bare-media-id")
	if !ok || p2.MediaID != "bare-media-id" {
		t.Fatalf("bare: %+v", p2)
	}
	if _, ok := DecodeMediaPayload(""); ok {
		t.Fatal("empty should fail")
	}
	if _, ok := DecodeMediaPayload(`{"media_id":""}`); ok {
		t.Fatal("empty media_id should fail")
	}
}

func TestDeliverableMsgType(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"text", "image", "file", "voice", "IMAGE"} {
		if !DeliverableMsgType(typ) {
			t.Fatalf("%s should be deliverable", typ)
		}
	}
	for _, typ := range []string{"video", "location", "event", ""} {
		if DeliverableMsgType(typ) {
			t.Fatalf("%s should not be deliverable", typ)
		}
	}
}

func TestMarshalInboundFileXML(t *testing.T) {
	t.Parallel()
	b, err := marshalInbound(InboundMessage{
		FromUserName: "u1",
		CreateTime:   123,
		MsgType:      "file",
		MsgID:        "9",
		AgentID:      "1002",
		MediaID:      "mid",
		FileName:     SafeFileName("../dir/doc.pdf"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "<MsgType>file</MsgType>") || !strings.Contains(s, "<MediaId>mid</MediaId>") || !strings.Contains(s, "<FileName>doc.pdf</FileName>") {
		t.Fatalf("xml=%s", s)
	}
	var back InboundMessage
	if err := xml.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.FileName != "doc.pdf" || back.MediaID != "mid" {
		t.Fatalf("%+v", back)
	}
}

func TestTruncateID(t *testing.T) {
	t.Parallel()
	if TruncateID("abcdef", 8) != "abcdef" {
		t.Fatal("short")
	}
	if TruncateID("abcdefghij", 8) != "abcdefgh…" {
		t.Fatal("long")
	}
}
