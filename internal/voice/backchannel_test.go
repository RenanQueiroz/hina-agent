package voice

import "testing"

func TestBackchannelIsBackchannel(t *testing.T) {
	b := NewBackchannel(nil, 0, true)
	pure := []string{"", "yeah", "Yeah,", "uh-huh", "uh huh", "okay sure", "mm-hmm yeah", "thank you", "got it"}
	for _, s := range pure {
		if !b.IsBackchannel(s) {
			t.Errorf("IsBackchannel(%q) = false, want true", s)
		}
	}
	real := []string{"what time is it", "yeah but what about tuesday", "stop the music", "okay play jazz"}
	for _, s := range real {
		if b.IsBackchannel(s) {
			t.Errorf("IsBackchannel(%q) = true, want false (has real content)", s)
		}
	}
}

func TestBackchannelInterrupts(t *testing.T) {
	b := NewBackchannel(nil, 2, true) // >=2 non-backchannel words confirms
	if b.Interrupts("yeah") || b.Interrupts("uh huh okay") {
		t.Fatal("pure backchannel must not interrupt")
	}
	if b.Interrupts("stop") {
		t.Fatal("a single non-backchannel word is below minWords=2")
	}
	if !b.Interrupts("stop playing") {
		t.Fatal("two non-backchannel words should interrupt")
	}
	if !b.Interrupts("yeah stop the music") {
		t.Fatal("backchannel prefix + real request should interrupt")
	}
}

func TestBackchannelDisabledIsAggressive(t *testing.T) {
	b := NewBackchannel(nil, 2, false)
	if b.IsBackchannel("yeah") {
		t.Fatal("disabled filter: only empty text is a backchannel")
	}
	if !b.IsBackchannel("") {
		t.Fatal("disabled filter: empty is still a backchannel")
	}
	if !b.Interrupts("yeah") {
		t.Fatal("disabled filter: any non-empty partial interrupts")
	}
}

func TestBackchannelCustomPhrases(t *testing.T) {
	b := NewBackchannel([]string{"roger", "copy that"}, 1, true)
	if !b.IsBackchannel("roger") || !b.IsBackchannel("copy that") {
		t.Fatal("custom phrases should be recognized")
	}
	if b.IsBackchannel("yeah") {
		t.Fatal("custom list replaces defaults: 'yeah' is no longer a backchannel")
	}
}
