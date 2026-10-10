package wecom

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ClientMessage is the clear XML sent to cc-connect's callback endpoint.
type ClientMessage struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content,omitempty"`
	MsgID        int64    `xml:"MsgId"`
	AgentID      string   `xml:"AgentID,omitempty"`
}

func (m ClientMessage) XML() ([]byte, error) {
	if strings.TrimSpace(m.ToUserName) == "" || strings.TrimSpace(m.FromUserName) == "" || m.CreateTime <= 0 || m.MsgID <= 0 {
		return nil, fmt.Errorf("%w: invalid client message", ErrInvalidArgument)
	}
	return xml.Marshal(m)
}
func BuildClientMessage(toUser, fromUser, content, agentID string, createTime time.Time, msgID int64) ([]byte, error) {
	if createTime.IsZero() {
		return nil, fmt.Errorf("%w: create time required", ErrInvalidArgument)
	}
	return (ClientMessage{ToUserName: toUser, FromUserName: fromUser, CreateTime: createTime.Unix(), MsgType: "text", Content: content, MsgID: msgID, AgentID: agentID}).XML()
}

type SignedCallback struct {
	Body      []byte
	Signature string
	Timestamp string
	Nonce     string
}

func (w *Webhook) BuildClientCallback(msg ClientMessage, timestamp, nonce string) (SignedCallback, error) {
	inner, err := msg.XML()
	if err != nil {
		return SignedCallback{}, err
	}
	enc, err := Encrypt(w.AESKey, string(inner), w.Receiver)
	if err != nil {
		return SignedCallback{}, err
	}
	now := time.Now()
	if w != nil && w.Clock != nil {
		now = w.Clock()
	}
	if timestamp == "" {
		timestamp = strconv.FormatInt(now.Unix(), 10)
	}
	if nonce == "" {
		nonce = strconv.FormatInt(now.UnixNano(), 10)
	}
	body, err := marshalEnvelope(w.Receiver, msg.AgentID, enc)
	if err != nil {
		return SignedCallback{}, err
	}
	return SignedCallback{Body: body, Signature: w.Signature(timestamp, nonce, enc), Timestamp: timestamp, Nonce: nonce}, nil
}
