package graph

import "testing"

func TestBuildGraphMessageReplyShape(t *testing.T) {
	m := buildGraphMessage("box@contoso.com", "Box", "a@ex.com", "A", "Re: ticket", "<p>hi</p>", "reply@ex.com", "Reply")
	if _, ok := m["internetMessageHeaders"]; ok {
		t.Fatal("internetMessageHeaders must be omitted; Graph rejects names that do not start with x-")
	}
	replyTo, ok := m["replyTo"].([]map[string]any)
	if !ok || len(replyTo) != 1 {
		t.Fatalf("replyTo = %#v, want a one-element recipient array", m["replyTo"])
	}
	addr, _ := replyTo[0]["emailAddress"].(map[string]string)
	if addr["address"] != "reply@ex.com" || addr["name"] != "Reply" {
		t.Fatalf("replyTo recipient = %#v", replyTo[0]["emailAddress"])
	}
	if _, ok := m["toRecipients"].([]map[string]any); !ok {
		t.Fatalf("toRecipients = %#v, want a recipient array", m["toRecipients"])
	}
}
