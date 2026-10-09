package bridge

import (
	"encoding/xml"
	"fmt"
)

// InboundMessage is the plaintext message forwarded to cc-connect.
type InboundMessage struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName,omitempty"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content,omitempty"`
	MsgID        string   `xml:"MsgId,omitempty"`
	AgentID      string   `xml:"AgentID,omitempty"`
	Event        string   `xml:"Event,omitempty"`
	EventKey     string   `xml:"EventKey,omitempty"`
	MediaID      string   `xml:"MediaId,omitempty"`
	Format       string   `xml:"Format,omitempty"`
	RawXML       string   `xml:"-"`
}

type callbackXML struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName,omitempty"`
	FromUserName string   `xml:"FromUserName,omitempty"`
	CreateTime   int64    `xml:"CreateTime,omitempty"`
	MsgType      string   `xml:"MsgType,omitempty"`
	Content      string   `xml:"Content,omitempty"`
	MsgID        string   `xml:"MsgId,omitempty"`
	AgentID      string   `xml:"AgentID,omitempty"`
	Event        string   `xml:"Event,omitempty"`
	EventKey     string   `xml:"EventKey,omitempty"`
	MediaID      string   `xml:"MediaId,omitempty"`
	Format       string   `xml:"Format,omitempty"`
}

type CallbackEnvelope struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName,omitempty"`
	AgentID    string   `xml:"AgentID,omitempty"`
	Encrypt    string   `xml:"Encrypt"`
}
type Callback struct {
	Signature string
	Timestamp string
	Nonce     string
	Body      []byte
	Encrypted string
}

func marshalInbound(m InboundMessage) ([]byte, error) {
	return xml.Marshal(callbackXML{ToUserName: m.ToUserName, FromUserName: m.FromUserName, CreateTime: m.CreateTime, MsgType: m.MsgType, Content: m.Content, MsgID: m.MsgID, AgentID: m.AgentID, Event: m.Event, EventKey: m.EventKey, MediaID: m.MediaID, Format: m.Format})
}
func unmarshalInbound(b []byte) (InboundMessage, error) {
	var m InboundMessage
	if err := xml.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("bridge: decode callback XML: %w", err)
	}
	m.RawXML = string(b)
	return m, nil
}
func marshalEnvelope(e CallbackEnvelope) ([]byte, error) { return xml.Marshal(e) }
func unmarshalEnvelope(b []byte) (CallbackEnvelope, error) {
	var e CallbackEnvelope
	if err := xml.Unmarshal(b, &e); err != nil {
		return e, err
	}
	if e.Encrypt == "" {
		return e, fmt.Errorf("bridge: callback envelope missing Encrypt")
	}
	return e, nil
}
