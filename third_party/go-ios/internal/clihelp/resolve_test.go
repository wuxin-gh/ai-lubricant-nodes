package clihelp

import "testing"

func TestResolveHelp_ExplicitAndImplicitParity(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("load help catalog: %v", err)
	}
	topicA, handledA := c.ResolveHelp([]string{"help", "tunnel", "start"})
	topicB, handledB := c.ResolveHelp([]string{"tunnel", "start", "--help"})
	if !handledA || !handledB {
		t.Fatal("expected help to be handled for both forms")
	}
	if topicA != topicB {
		t.Fatalf("topic mismatch: explicit=%q implicit=%q", topicA, topicB)
	}
}

func TestResolveHelp_StripsGlobalFlags(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("load help catalog: %v", err)
	}
	topic, handled := c.ResolveHelp([]string{"--udid=abc", "help", "apps"})
	if !handled {
		t.Fatal("expected help to be handled")
	}
	if topic != "apps" {
		t.Fatalf("topic = %q, want apps", topic)
	}
}

func TestResolveHelp_UnknownTopic(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("load help catalog: %v", err)
	}
	topic, handled := c.ResolveHelp([]string{"help", "nope"})
	if !handled {
		t.Fatal("expected help to be handled")
	}
	if topic != "nope" {
		t.Fatalf("topic = %q, want nope", topic)
	}
}
