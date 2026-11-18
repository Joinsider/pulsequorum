// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package tenant

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func intp(v int) *int { return &v }

func ptrIntEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func limitsEqual(a, b Limits) bool {
	return ptrIntEqual(a.MaxMonitors, b.MaxMonitors) &&
		ptrIntEqual(a.MinIntervalSeconds, b.MinIntervalSeconds) &&
		ptrIntEqual(a.MaxChannels, b.MaxChannels) &&
		ptrIntEqual(a.MaxStatusPages, b.MaxStatusPages) &&
		ptrIntEqual(a.MaxPrivateProbes, b.MaxPrivateProbes)
}

// overridesEqual distinguishes an absent name from a name mapped to nil.
func overridesEqual(a, b Overrides) bool {
	if len(a) != len(b) {
		return false
	}
	for name, av := range a {
		bv, ok := b[name]
		if !ok || !ptrIntEqual(av, bv) {
			return false
		}
	}
	return true
}

// invalidInputs must be rejected by both ParseLimits and ParseOverrides.
// errSub is a substring the error message must contain; empty means no check.
var invalidInputs = []struct {
	name   string
	in     string
	errSub string
}{
	{"unknown field", `{"maxUsers":1}`, "maxUsers"},
	{"unknown field next to valid field", `{"maxMonitors":1,"maxUsers":1}`, "maxUsers"},
	{"unknown field with null", `{"foo":null}`, "foo"},
	{"non-integer 1.5", `{"maxMonitors":1.5}`, "maxMonitors"},
	{"quoted number", `{"maxMonitors":"3"}`, "maxMonitors"},
	{"bool", `{"maxMonitors":true}`, "maxMonitors"},
	{"array value", `{"maxMonitors":[]}`, "maxMonitors"},
	{"object value", `{"maxMonitors":{}}`, "maxMonitors"},
	{"negative", `{"maxMonitors":-1}`, "maxMonitors"},
	{"negative minIntervalSeconds", `{"minIntervalSeconds":-1}`, "minIntervalSeconds"},
	{"minIntervalSeconds zero", `{"minIntervalSeconds":0}`, "minIntervalSeconds"},
	{"above upper bound", `{"maxMonitors":1000001}`, "maxMonitors"},
	{"exponent 1e30", `{"maxMonitors":1e30}`, "maxMonitors"},
	{"integer beyond int64", `{"maxMonitors":99999999999999999999}`, "maxMonitors"},
	{"top-level array", `[]`, ""},
	{"top-level number", `3`, ""},
	{"top-level string", `"x"`, ""},
	{"trailing second object", `{} {}`, ""},
	{"trailing closing brace", `{}}`, ""},
	{"trailing closing bracket", `{}]`, ""},
	{"truncated object", `{"maxMonitors":1`, ""},
}

func checkInvalid(t *testing.T, err error, errSub string) {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, ErrInvalidLimits) {
		t.Errorf("error %q does not wrap ErrInvalidLimits", err)
	}
	if errSub != "" && !strings.Contains(err.Error(), errSub) {
		t.Errorf("error %q does not contain %q", err, errSub)
	}
}

func TestParseLimits(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Limits
	}{
		{"empty input", "", Limits{}},
		{"whitespace only", " \t\r\n ", Limits{}},
		{"null", "null", Limits{}},
		{"empty object", "{}", Limits{}},
		{"all five valid", `{"maxMonitors":10,"minIntervalSeconds":60,"maxChannels":3,"maxStatusPages":2,"maxPrivateProbes":1}`,
			Limits{MaxMonitors: intp(10), MinIntervalSeconds: intp(60), MaxChannels: intp(3), MaxStatusPages: intp(2), MaxPrivateProbes: intp(1)}},
		{"maxMonitors only", `{"maxMonitors":5}`, Limits{MaxMonitors: intp(5)}},
		{"minIntervalSeconds only", `{"minIntervalSeconds":5}`, Limits{MinIntervalSeconds: intp(5)}},
		{"maxChannels only", `{"maxChannels":7}`, Limits{MaxChannels: intp(7)}},
		{"maxStatusPages only", `{"maxStatusPages":7}`, Limits{MaxStatusPages: intp(7)}},
		{"maxPrivateProbes only", `{"maxPrivateProbes":7}`, Limits{MaxPrivateProbes: intp(7)}},
		{"null field is unlimited", `{"maxMonitors":null,"minIntervalSeconds":5}`, Limits{MinIntervalSeconds: intp(5)}},
		{"maxMonitors zero allowed", `{"maxMonitors":0}`, Limits{MaxMonitors: intp(0)}},
		{"minIntervalSeconds one allowed", `{"minIntervalSeconds":1}`, Limits{MinIntervalSeconds: intp(1)}},
		{"upper bound allowed", `{"maxChannels":1000000}`, Limits{MaxChannels: intp(1000000)}},
		{"whitespace around object", "  {\"maxMonitors\":4}\n ", Limits{MaxMonitors: intp(4)}},
		// Duplicate keys: encoding/json keeps the last value. Documented behavior.
		{"duplicate key, last wins", `{"maxMonitors":1,"maxMonitors":2}`, Limits{MaxMonitors: intp(2)}},
		{"duplicate key, null last", `{"maxMonitors":1,"maxMonitors":null}`, Limits{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLimits([]byte(tt.in))
			if err != nil {
				t.Fatalf("ParseLimits(%q) error: %v", tt.in, err)
			}
			if !limitsEqual(got, tt.want) {
				t.Errorf("ParseLimits(%q) = %s, want %s", tt.in, got.Canonical(), tt.want.Canonical())
			}
		})
	}
}

func TestParseLimitsRejects(t *testing.T) {
	for _, tt := range invalidInputs {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseLimits([]byte(tt.in))
			checkInvalid(t, err, tt.errSub)
		})
	}
}

func TestParseOverrides(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Overrides
	}{
		{"empty input", "", Overrides{}},
		{"whitespace only", " \t\r\n ", Overrides{}},
		{"null", "null", Overrides{}},
		{"empty object", "{}", Overrides{}},
		{"all five valid", `{"maxMonitors":10,"minIntervalSeconds":60,"maxChannels":3,"maxStatusPages":2,"maxPrivateProbes":1}`,
			Overrides{LimitMonitors: intp(10), LimitMinInterval: intp(60), LimitChannels: intp(3), LimitStatusPages: intp(2), LimitPrivateProbes: intp(1)}},
		{"null value kept as nil", `{"maxMonitors":null}`, Overrides{LimitMonitors: nil}},
		{"all five null", `{"maxMonitors":null,"minIntervalSeconds":null,"maxChannels":null,"maxStatusPages":null,"maxPrivateProbes":null}`,
			Overrides{LimitMonitors: nil, LimitMinInterval: nil, LimitChannels: nil, LimitStatusPages: nil, LimitPrivateProbes: nil}},
		{"mixed null and value", `{"maxMonitors":null,"maxChannels":2}`, Overrides{LimitMonitors: nil, LimitChannels: intp(2)}},
		{"minIntervalSeconds one allowed", `{"minIntervalSeconds":1}`, Overrides{LimitMinInterval: intp(1)}},
		{"maxMonitors zero allowed", `{"maxMonitors":0}`, Overrides{LimitMonitors: intp(0)}},
		{"upper bound allowed", `{"maxChannels":1000000}`, Overrides{LimitChannels: intp(1000000)}},
		{"duplicate key, last wins", `{"maxMonitors":1,"maxMonitors":2}`, Overrides{LimitMonitors: intp(2)}},
		{"duplicate key, null last", `{"maxMonitors":1,"maxMonitors":null}`, Overrides{LimitMonitors: nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOverrides([]byte(tt.in))
			if err != nil {
				t.Fatalf("ParseOverrides(%q) error: %v", tt.in, err)
			}
			if !overridesEqual(got, tt.want) {
				t.Errorf("ParseOverrides(%q) = %s, want %s", tt.in, got.Canonical(), tt.want.Canonical())
			}
		})
	}
}

func TestParseOverridesNullKeepsKey(t *testing.T) {
	o, err := ParseOverrides([]byte(`{"maxMonitors":null}`))
	if err != nil {
		t.Fatalf("ParseOverrides error: %v", err)
	}
	v, ok := o[LimitMonitors]
	if !ok {
		t.Fatal("null override: key absent, want present with nil value")
	}
	if v != nil {
		t.Errorf("null override: value %d, want nil", *v)
	}
	// The same input means "unlimited" for plan limits, where absence and null are equal.
	l, err := ParseLimits([]byte(`{"maxMonitors":null}`))
	if err != nil {
		t.Fatalf("ParseLimits error: %v", err)
	}
	if l.MaxMonitors != nil {
		t.Errorf("ParseLimits null: MaxMonitors = %d, want nil", *l.MaxMonitors)
	}
}

func TestParseOverridesRejects(t *testing.T) {
	for _, tt := range invalidInputs {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseOverrides([]byte(tt.in))
			checkInvalid(t, err, tt.errSub)
		})
	}
}

func TestEffective(t *testing.T) {
	plan := Limits{
		MaxMonitors:        intp(10),
		MinIntervalSeconds: intp(60),
		MaxChannels:        nil,
		MaxStatusPages:     intp(2),
		MaxPrivateProbes:   nil,
	}
	tests := []struct {
		name string
		plan Limits
		o    Overrides
		want Limits
	}{
		{"nil overrides keep plan", plan, nil, plan},
		{"empty overrides keep plan", plan, Overrides{}, plan},
		{"absent name keeps plan value",
			plan, Overrides{LimitChannels: intp(5)},
			Limits{MaxMonitors: intp(10), MinIntervalSeconds: intp(60), MaxChannels: intp(5), MaxStatusPages: intp(2), MaxPrivateProbes: nil}},
		{"override replaces plan value",
			plan, Overrides{LimitMonitors: intp(20)},
			Limits{MaxMonitors: intp(20), MinIntervalSeconds: intp(60), MaxStatusPages: intp(2)}},
		{"override can set a stricter value",
			plan, Overrides{LimitMinInterval: intp(1)},
			Limits{MaxMonitors: intp(10), MinIntervalSeconds: intp(1), MaxStatusPages: intp(2)}},
		{"nil override lifts plan limit",
			plan, Overrides{LimitMonitors: nil},
			Limits{MinIntervalSeconds: intp(60), MaxStatusPages: intp(2)}},
		{"nil override on unlimited field stays unlimited",
			plan, Overrides{LimitPrivateProbes: nil},
			Limits{MaxMonitors: intp(10), MinIntervalSeconds: intp(60), MaxStatusPages: intp(2)}},
		{"all five overridden",
			plan, Overrides{LimitMonitors: intp(1), LimitMinInterval: intp(2), LimitChannels: intp(3), LimitStatusPages: nil, LimitPrivateProbes: intp(5)},
			Limits{MaxMonitors: intp(1), MinIntervalSeconds: intp(2), MaxChannels: intp(3), MaxPrivateProbes: intp(5)}},
		{"unknown name ignored",
			plan, Overrides{"maxUsers": intp(1)},
			plan},
		{"unknown name with nil ignored",
			plan, Overrides{"bogus": nil},
			plan},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.plan.Canonical()
			got := Effective(tt.plan, tt.o)
			if !limitsEqual(got, tt.want) {
				t.Errorf("Effective = %s, want %s", got.Canonical(), tt.want.Canonical())
			}
			if after := tt.plan.Canonical(); string(after) != string(before) {
				t.Errorf("Effective modified the plan: %s -> %s", before, after)
			}
		})
	}
}

func TestLimitsCanonical(t *testing.T) {
	tests := []struct {
		name string
		l    Limits
		want string
	}{
		{"all nil", Limits{}, `{}`},
		{"all set in fixed order", Limits{
			MaxMonitors: intp(1), MinIntervalSeconds: intp(2), MaxChannels: intp(3), MaxStatusPages: intp(4), MaxPrivateProbes: intp(5),
		}, `{"maxMonitors":1,"minIntervalSeconds":2,"maxChannels":3,"maxStatusPages":4,"maxPrivateProbes":5}`},
		{"partial omits nil fields", Limits{MaxChannels: intp(3)}, `{"maxChannels":3}`},
		{"zero is kept", Limits{MaxMonitors: intp(0)}, `{"maxMonitors":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(tt.l.Canonical()); got != tt.want {
				t.Errorf("Canonical() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestOverridesCanonical(t *testing.T) {
	tests := []struct {
		name string
		o    Overrides
		want string
	}{
		{"nil map", nil, `{}`},
		{"empty map", Overrides{}, `{}`},
		{"nil value is null", Overrides{LimitMonitors: nil}, `{"maxMonitors":null}`},
		{"keys sorted", Overrides{LimitStatusPages: intp(3), LimitMonitors: nil}, `{"maxMonitors":null,"maxStatusPages":3}`},
		{"all five sorted", Overrides{
			LimitMonitors: intp(2), LimitMinInterval: intp(5), LimitChannels: intp(1), LimitStatusPages: intp(4), LimitPrivateProbes: nil,
		}, `{"maxChannels":1,"maxMonitors":2,"maxPrivateProbes":null,"maxStatusPages":4,"minIntervalSeconds":5}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(tt.o.Canonical()); got != tt.want {
				t.Errorf("Canonical() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCanonicalDeterministic(t *testing.T) {
	o := Overrides{LimitMonitors: intp(2), LimitMinInterval: intp(5), LimitChannels: nil, LimitStatusPages: intp(4), LimitPrivateProbes: intp(9)}
	l := Limits{MaxMonitors: intp(2), MinIntervalSeconds: intp(5), MaxStatusPages: intp(4)}
	wantO := string(o.Canonical())
	wantL := string(l.Canonical())
	for i := 0; i < 50; i++ {
		if got := string(o.Canonical()); got != wantO {
			t.Fatalf("Overrides.Canonical run %d = %s, want %s", i, got, wantO)
		}
		if got := string(l.Canonical()); got != wantL {
			t.Fatalf("Limits.Canonical run %d = %s, want %s", i, got, wantL)
		}
	}
}

func TestLimitsRoundTrip(t *testing.T) {
	tests := []Limits{
		{},
		{MaxMonitors: intp(0)},
		{MaxMonitors: intp(1), MinIntervalSeconds: intp(1)},
		{MaxChannels: intp(1000000), MaxPrivateProbes: intp(12), MaxStatusPages: nil},
		{MaxMonitors: intp(10), MinIntervalSeconds: intp(60), MaxChannels: intp(3), MaxStatusPages: intp(2), MaxPrivateProbes: intp(1)},
	}
	for _, l := range tests {
		got, err := ParseLimits(l.Canonical())
		if err != nil {
			t.Fatalf("ParseLimits(%s) error: %v", l.Canonical(), err)
		}
		if !limitsEqual(got, l) {
			t.Errorf("round trip of %s = %s", l.Canonical(), got.Canonical())
		}
	}
}

func TestOverridesRoundTrip(t *testing.T) {
	tests := []Overrides{
		nil,
		{},
		{LimitMonitors: nil},
		{LimitMonitors: intp(0)},
		{LimitMinInterval: intp(1), LimitChannels: nil},
		{LimitMonitors: intp(10), LimitMinInterval: intp(60), LimitChannels: nil, LimitStatusPages: intp(2), LimitPrivateProbes: intp(1000000)},
	}
	for _, o := range tests {
		got, err := ParseOverrides(o.Canonical())
		if err != nil {
			t.Fatalf("ParseOverrides(%s) error: %v", o.Canonical(), err)
		}
		if !overridesEqual(got, o) {
			t.Errorf("round trip of %s = %s", o.Canonical(), got.Canonical())
		}
	}
}

func TestValidName(t *testing.T) {
	tests := []struct {
		desc string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"single letter", "a", true},
		{"100 runes of two-byte ä", strings.Repeat("ä", 100), true},
		{"101 runes of two-byte ä", strings.Repeat("ä", 101), false},
		{"100 ASCII runes", strings.Repeat("a", 100), true},
		{"101 ASCII runes", strings.Repeat("a", 101), false},
		{"leading space", " a", false},
		{"trailing space", "a ", false},
		{"leading tab", "\ta", false},
		{"tab inside", "a\tb", false},
		{"newline inside", "a\nb", false},
		{"NUL", "a\x00b", false},
		{"DEL", "a\x7fb", false},
		{"NEL U+0085", "a\u0085b", false},
		{"line separator U+2028", "a\u2028b", false},
		{"paragraph separator U+2029", "a\u2029b", false},
		{"zero-width joiner U+200D", "a\u200db", false},
		{"zero-width space U+200B", "a\u200bb", false},
		{"byte order mark U+FEFF", "a\ufeffb", false},
		{"soft hyphen U+00AD", "a\u00adb", false},
		{"bidi override U+202E", "a\u202eb", false},
		{"invalid UTF-8", "a\xffb", false},
		{"truncated UTF-8 sequence", "a\xc3", false},
		{"umlauts", "Müller Öl Äpfel", true},
		{"inner spaces", "Acme Corp GmbH", true},
		{"emoji", "Team 😀", true},
		{"punctuation", "Müller & Söhne (DE)", true},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := ValidName(tt.in); got != tt.want {
				t.Errorf("ValidName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		desc     string
		in       string
		fallback string
		want     string
	}{
		{"collapses and trims white space", "  Acme \t  Corp\n", "fb", "Acme Corp"},
		{"control characters dropped without a space", "a\x00b\x07c", "fb", "abc"},
		{"format characters dropped", "a\u200db\u200bc\ufeffd\u00ade", "fb", "abcde"},
		{"U+2028 is a space", "a\u2028b", "fb", "a b"},
		{"umlauts kept", "Müller GmbH", "fb", "Müller GmbH"},
		{"empty uses fallback", "", "fb", "fb"},
		{"only white space uses fallback", " \t\n ", "fb", "fb"},
		{"only format characters uses fallback", "\u200b\u200d", "fb", "fb"},
		{"invalid UTF-8 uses fallback", "ok\xff", "fb", "fb"},
		{"cut to 100 runes", strings.Repeat("ä", 150), "fb", strings.Repeat("ä", 100)},
		{"no trailing space after cut", strings.Repeat("a", 99) + " b", "fb", strings.Repeat("a", 99)},
		{"exactly 100 runes kept", strings.Repeat("a", 100) + " b", "fb", strings.Repeat("a", 100)},
		{"space before cut is dropped", strings.Repeat("a", 50) + " " + strings.Repeat("b", 60), "fb",
			strings.Repeat("a", 50) + " " + strings.Repeat("b", 49)},
		{"fallback returned as given", "", "Organisation", "Organisation"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := SanitizeName(tt.in, tt.fallback); got != tt.want {
				t.Errorf("SanitizeName(%q, %q) = %q, want %q", tt.in, tt.fallback, got, tt.want)
			}
		})
	}
}

// TestSanitizeNameProperty checks for every tricky input that the result is
// either a valid name or the fallback.
func TestSanitizeNameProperty(t *testing.T) {
	pieces := []string{" ", "\t", "\n", "\x00", "\x7f", "a", "b", "ä", "\u00a0", "\u0085", "\u2028", "\u2029",
		"\u200b", "\u200d", "\u00ad", "\ufeff", "\U0001F600", "\xff", "\u202e"}
	inputs := []string{""}
	for _, a := range pieces {
		inputs = append(inputs, a)
		for _, b := range pieces {
			inputs = append(inputs, a+b)
			for _, c := range pieces {
				inputs = append(inputs, a+b+c)
			}
		}
	}
	inputs = append(inputs,
		strings.Repeat("a", 99)+" b",
		strings.Repeat("ä", 100)+" x",
		strings.Repeat("a ", 120),
		strings.Repeat(" ", 200)+"x",
		strings.Repeat("\u200b", 200),
		strings.Repeat("ä\u2028", 80),
	)
	fallbacks := []string{"fallback", "", " ", "\xff"}
	for _, fb := range fallbacks {
		for _, in := range inputs {
			out := SanitizeName(in, fb)
			if out != fb && !ValidName(out) {
				t.Fatalf("SanitizeName(%q, %q) = %q: neither the fallback nor ValidName", in, fb, out)
			}
			if n := utf8.RuneCountInString(out); n > maxNameLength {
				t.Fatalf("SanitizeName(%q, %q) = %d runes, want at most %d", in, fb, n, maxNameLength)
			}
		}
	}
}

func TestValidID(t *testing.T) {
	tests := []struct {
		desc string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"lower-case letters", "abc", true},
		{"digit", "0", true},
		{"leading digit", "1abc", true},
		{"inner hyphens", "a-b-1", true},
		{"UUID", "123e4567-e89b-12d3-a456-426614174000", true},
		{"default", "default", true},
		{"reserved _system", "_system", false},
		{"upper case", "Abc", false},
		{"single upper case", "A", false},
		{"leading hyphen", "-a", false},
		{"hyphen alone", "-", false},
		{"underscore", "a_b", false},
		{"dot", "a.b", false},
		{"space", "a b", false},
		{"umlaut", "ä", false},
		{"newline", "a\n", false},
		{"64 bytes", strings.Repeat("a", 64), true},
		{"65 bytes", strings.Repeat("a", 65), false},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := ValidID(tt.in); got != tt.want {
				t.Errorf("ValidID(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestValidOrgID mirrors TestValidOrgID in internal/cluster/tenants_test.go and adds the length bounds.
func TestValidOrgID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"", false},
		{"1", true},
		{"123456789012345678", true},
		{"a.b_c@d-e", true},
		{strings.Repeat("x", 128), true},
		{strings.Repeat("x", 128+1), false},
		{"a b", false},
		{"a/b", false},
		{"a:b", false},
		{"a+b", false},
		{"äb", false},
		{"a\x00b", false},
		{"a\nb", false},
		{strings.Repeat("ä", 64), false}, // 128 bytes, but not ASCII
	}
	for _, tt := range tests {
		if got := ValidOrgID(tt.id); got != tt.want {
			t.Errorf("ValidOrgID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestProtected(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"_system", true},
		{"default", true},
		{"acme", false},
		{"system", false},
		{"", false},
		{"Default", false},
	}
	for _, tt := range tests {
		if got := Protected(tt.id); got != tt.want {
			t.Errorf("Protected(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func FuzzParseOverrides(f *testing.F) {
	for _, s := range []string{
		"", "null", "{}", " ", `{"maxMonitors":null}`, `{"maxMonitors":1}`, `{"minIntervalSeconds":1}`,
		`{"maxPrivateProbes":1000000}`, `{"maxChannels":1e3}`, `{"maxMonitors":"3"}`, `{"maxUsers":1}`,
		`{} {}`, `{}}`, `[]`, `3`, `"x"`, `{"maxMonitors":-1}`, `{"maxMonitors":1,"maxMonitors":null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		o, err := ParseOverrides(data)
		if err != nil {
			if !errors.Is(err, ErrInvalidLimits) {
				t.Fatalf("ParseOverrides(%q) error does not wrap ErrInvalidLimits: %v", data, err)
			}
			return
		}
		c := o.Canonical()
		o2, err := ParseOverrides(c)
		if err != nil {
			t.Fatalf("ParseOverrides(%q) accepted, but its Canonical %q is rejected: %v", data, c, err)
		}
		if !overridesEqual(o, o2) {
			t.Fatalf("round trip of %q: %s became %s", data, c, o2.Canonical())
		}
	})
}
