package config

import (
	"testing"
	"time"
)

func TestPriorityAgeAfter(t *testing.T) {
	for _, c := range []struct {
		name, host string
		want       time.Duration
	}{
		{"default", "version = 1\n", 5 * time.Minute},
		{"set", "version = 1\n[priority]\nage_after = \"2m\"\n", 2 * time.Minute},
		{"zero turns aging off", "version = 1\n[priority]\nage_after = \"0\"\n", 0},
		{"zero with a unit", "version = 1\n[priority]\nage_after = \"0s\"\n", 0},
		{"next to the cwd lists", "version = 1\n[priority]\nhigh_cwd_contains = [\"x\"]\nage_after = \"1h\"\n", time.Hour},
	} {
		e := mustLoad(t, newFixture(t, c.host, ""))
		if e.Priority.AgeAfter != c.want {
			t.Errorf("%s: AgeAfter = %v, want %v", c.name, e.Priority.AgeAfter, c.want)
		}
	}
}

func TestPriorityAgeAfterBadValueIs78(t *testing.T) {
	for _, v := range []string{`"soon"`, `"-5m"`, `"10"`, `5`} {
		_, err := newFixture(t, "version = 1\n[priority]\nage_after = "+v+"\n", "").load()
		expect78(t, err, "priority.age_after")
	}
}
