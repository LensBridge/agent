package offline

import (
	"encoding/json"
	"testing"
	"time"
)

func mustManifest(t *testing.T, tz, first, last string) *Manifest {
	t.Helper()
	raw, _ := json.Marshal(Manifest{
		FormatVersion: 1, DeviceID: testDevice, Timezone: tz,
		GeneratedAt: "2026-09-24T14:02:11Z", FirstDay: first, LastDay: last,
	})
	m, err := parseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPickDay(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const first, last = "2026-09-24", "2026-10-07"

	cases := []struct {
		name      string
		tz        string
		first     string
		last      string
		now       string
		today     string
		serving   string
		remaining int
		stale     int
	}{
		{"mid-range", "America/Toronto", first, last, "2026-09-30T16:00:00Z", "2026-09-30", "2026-09-30", 7, 0},
		// 23:59 EDT on the 23rd is already the 24th in UTC; the zone decides.
		{"before first, zone decides", "America/Toronto", first, last, "2026-09-24T03:59:00Z", "2026-09-23", first, 14, 0},
		{"first day at local midnight", "America/Toronto", first, last, "2026-09-24T04:00:00Z", first, first, 13, 0},
		{"well before first", "America/Toronto", first, last, "2026-09-01T12:00:00Z", "2026-09-01", first, 36, 0},
		{"last day, evening", "America/Toronto", first, last, "2026-10-08T03:59:00Z", last, last, 0, 0},
		{"one day stale", "America/Toronto", first, last, "2026-10-08T04:00:00Z", "2026-10-08", last, 0, 1},
		{"long stale", "America/Toronto", first, last, "2026-10-20T12:00:00Z", "2026-10-20", last, 0, 13},

		// The same instant is a different date in different zones.
		{"UTC+14 is a day ahead", "Pacific/Kiritimati", first, last, "2026-09-30T11:00:00Z", "2026-10-01", "2026-10-01", 6, 0},
		{"UTC-11 is a day behind", "Pacific/Pago_Pago", first, last, "2026-09-30T09:00:00Z", "2026-09-29", "2026-09-29", 8, 0},
		{"half-hour zone", "Asia/Kolkata", first, last, "2026-10-07T18:29:00Z", last, last, 0, 0},
		{"half-hour zone rolls over", "Asia/Kolkata", first, last, "2026-10-07T18:30:00Z", "2026-10-08", last, 0, 1},

		// Across the Nov 1 fall-back, when one local day is 25 hours long.
		{"across DST end", "America/Toronto", "2026-10-25", "2026-11-07", "2026-11-02T12:00:00Z", "2026-11-02", "2026-11-02", 5, 0},
		{"on DST end day, late", "America/Toronto", "2026-10-25", "2026-11-07", "2026-11-02T04:30:00Z", "2026-11-01", "2026-11-01", 6, 0},
		// And the spring-forward day, 23 hours long.
		{"across DST start", "America/Toronto", "2026-03-01", "2026-03-14", "2026-03-09T03:30:00Z", "2026-03-08", "2026-03-08", 6, 0},

		{"single-day bundle", "America/Toronto", first, first, "2026-09-24T12:00:00Z", first, first, 0, 0},
		{"single-day bundle, next day", "America/Toronto", first, first, "2026-09-25T12:00:00Z", "2026-09-25", first, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustManifest(t, tc.tz, tc.first, tc.last)
			got := PickDay(m, at(tc.now))
			want := Day{Today: tc.today, Serving: tc.serving, DaysRemaining: tc.remaining, StaleDays: tc.stale}
			if got != want {
				t.Errorf("PickDay = %+v, want %+v", got, want)
			}
		})
	}
}

// The instant's own zone must not matter, only the manifest's.
func TestPickDayIgnoresCallerZone(t *testing.T) {
	m := mustManifest(t, "America/Toronto", "2026-09-24", "2026-10-07")
	utc := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	if a, b := PickDay(m, utc), PickDay(m, utc.In(tokyo)); a != b {
		t.Errorf("%+v != %+v", a, b)
	}
}

func TestStatusFor(t *testing.T) {
	now := time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC)

	s := StatusFor("offline", testDevice, nil, now)
	raw, _ := json.Marshal(s)
	want := `{"mode":"offline","deviceId":"` + testDevice + `","bundle":null,"today":"2026-09-30","servingDay":null,"daysRemaining":null,"staleDays":null}`
	if string(raw) != want {
		t.Errorf("no-bundle status:\n got %s\nwant %s", raw, want)
	}

	s = StatusFor("offline", testDevice, mustManifest(t, "America/Toronto", "2026-09-24", "2026-10-07"), now)
	raw, _ = json.Marshal(s)
	want = `{"mode":"offline","deviceId":"` + testDevice + `","bundle":{"firstDay":"2026-09-24","lastDay":"2026-10-07","generatedAt":"2026-09-24T14:02:11Z","timezone":"America/Toronto"},"today":"2026-09-30","servingDay":"2026-09-30","daysRemaining":7,"staleDays":0}`
	if string(raw) != want {
		t.Errorf("status:\n got %s\nwant %s", raw, want)
	}
}
