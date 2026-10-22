package domain

import (
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
	"unicode"
)

var epoch = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func faultCode(t *testing.T, err error, want Code) {
	t.Helper()
	f, ok := FaultOf(err)
	if !ok || f.Code != want {
		t.Fatalf("error=%v, want code %s", err, want)
	}
	for _, r := range err.Error() {
		if unicode.In(r, unicode.Cyrillic) {
			t.Fatalf("technical error contains Cyrillic: %v", err)
		}
	}
}

func TestCampaignBoundaries(t *testing.T) {
	c := Campaign{Status: "active", StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}
	for _, tc := range []struct {
		name    string
		at      time.Time
		allowed bool
	}{{"before", epoch.Add(-time.Nanosecond), false}, {"start", epoch, true}, {"last", c.EndsAt.Add(-time.Nanosecond), true}, {"end", c.EndsAt, false}} {
		t.Run(tc.name, func(t *testing.T) {
			err := c.CanAct(tc.at)
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed {
				faultCode(t, err, NotActive)
			}
		})
	}
	c.Status = "closed"
	faultCode(t, c.CanAct(epoch), NotActive)
}

func TestDayBoundaries(t *testing.T) {
	d := Day{StartsAt: epoch, EndsAt: epoch.Add(time.Hour)}
	if err := d.CanAct(epoch); err != nil {
		t.Fatal(err)
	}
	faultCode(t, d.CanAct(epoch.Add(-time.Nanosecond)), NotActive)
	faultCode(t, d.CanAct(d.EndsAt), NotActive)
}

func TestGrantChance(t *testing.T) {
	d := Day{ID: "d1", Number: 1}
	grant, err := GrantChance(nil, d)
	if err != nil || !grant {
		t.Fatalf("grant=%v error=%v", grant, err)
	}
	active := []Activity{{DayID: "d1"}}
	grant, err = GrantChance(active, d)
	if err != nil || grant {
		t.Fatalf("duplicate grant=%v error=%v", grant, err)
	}
	active = nil
	for i := 1; i <= 10; i++ {
		active = append(active, Activity{DayID: fmt.Sprint(i)})
	}
	faultCode(t, mustGrantError(active, d), ChanceLimit)
	grant, err = GrantChance(active, Day{ID: "1", Number: 1})
	if err != nil || grant {
		t.Fatalf("duplicate at limit=%v %v", grant, err)
	}
	for _, n := range []int{0, 11} {
		faultCode(t, mustGrantError(nil, Day{ID: "bad", Number: n}), Invalid)
	}
}

func mustGrantError(a []Activity, d Day) error { _, err := GrantChance(a, d); return err }

func TestDrawTransitions(t *testing.T) {
	d := Draw{State: DrawOpen, ClosesAt: epoch, WinnerCount: 5, ReserveCount: 2, ClaimTTL: time.Hour}
	if err := d.CanRegister(epoch.Add(-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	faultCode(t, d.CanRegister(epoch), Closed)
	if err := d.CanDraw(epoch); err != nil {
		t.Fatal(err)
	}
	faultCode(t, d.CanDraw(epoch.Add(-time.Nanosecond)), InvalidTransition)
	faultCode(t, d.CanApprove(), InvalidTransition)
	d.State = DrawDrawn
	faultCode(t, d.CanRegister(epoch.Add(-time.Hour)), Closed)
	faultCode(t, d.CanDraw(epoch), InvalidTransition)
	if err := d.CanApprove(); err != nil {
		t.Fatal(err)
	}
	d.State = DrawApproved
	faultCode(t, d.CanApprove(), InvalidTransition)
	for _, change := range []func(*Draw){func(d *Draw) { d.WinnerCount = 0 }, func(d *Draw) { d.ReserveCount = -1 }, func(d *Draw) { d.ClaimTTL = 0 }} {
		invalid := Draw{State: DrawOpen, ClosesAt: epoch, WinnerCount: 1, ClaimTTL: time.Hour}
		change(&invalid)
		faultCode(t, invalid.CanDraw(epoch), Invalid)
	}
}

func TestOfferRules(t *testing.T) {
	deadline := epoch.Add(time.Hour)
	o := Offer{UserID: 42, Status: "notified", Deadline: &deadline}
	if err := o.CanClaim(42, "Иван Петров", true, epoch); err != nil {
		t.Fatal(err)
	}
	faultCode(t, o.CanClaim(41, "Иван Петров", true, epoch), Forbidden)
	faultCode(t, o.CanClaim(42, "Иван Петров", false, epoch), Invalid)
	faultCode(t, o.CanClaim(42, "И", true, epoch), Invalid)
	faultCode(t, o.CanClaim(42, "Иван Петров", true, deadline), Expired)
	if o.CanExpire(epoch) {
		t.Fatal("offer expired before its deadline")
	}
	if !o.CanExpire(deadline) {
		t.Fatal("offer not expired at deadline")
	}
	o.Status = "pending"
	o.Deadline = nil
	if o.CanExpire(deadline) {
		t.Fatal("unknown delivery started expiry")
	}
	if err := o.CanClaim(42, "Иван Петров", true, deadline); err != nil {
		t.Fatal("owner can confirm an ambiguously delivered offer:", err)
	}
	o.Status = "claimed"
	o.Deadline = &deadline
	if o.CanExpire(deadline) {
		t.Fatal("claimed offer expired")
	}
	if err := o.CanClaim(42, "Иван Петров", true, deadline); err != nil {
		t.Fatal(err)
	}
	o.Status = "expired"
	faultCode(t, o.CanClaim(42, "Иван Петров", true, epoch), Expired)
	o.Status = "invalid"
	faultCode(t, o.CanClaim(42, "Иван Петров", true, epoch), InvalidTransition)
	long := make([]rune, 121)
	for i := range long {
		long[i] = 'а'
	}
	o.Status = "pending"
	faultCode(t, o.CanClaim(42, string(long), true, epoch), Invalid)
}

type fixedRandom struct {
	value int64
	err   error
}

func (r fixedRandom) Intn(int64) (int64, error) { return r.value, r.err }

func TestWeightedSelectionBoundaries(t *testing.T) {
	pool := []Candidate{{UserID: 1, Weight: 1}, {UserID: 2, Weight: 7}}
	for _, tc := range []struct {
		value int64
		want  int64
	}{{0, 1}, {1, 2}, {7, 2}} {
		got, err := Select(pool, 1, 0, fixedRandom{value: tc.value})
		if err != nil || got.Winners[0] != tc.want {
			t.Fatalf("pick %d: %+v %v", tc.value, got, err)
		}
	}
	if !reflect.DeepEqual(pool, []Candidate{{UserID: 1, Weight: 1}, {UserID: 2, Weight: 7}}) {
		t.Fatal("selection mutated caller's candidate snapshot")
	}
	for _, value := range []int64{-1, 8} {
		_, err := Select(pool, 1, 0, fixedRandom{value: value})
		faultCode(t, err, Invalid)
	}
	want := errors.New("entropy unavailable")
	_, err := Select(pool, 1, 0, fixedRandom{err: want})
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func TestSelectionIsDistinctAndHandlesSmallPools(t *testing.T) {
	pool := []Candidate{{1, 1}, {2, 2}, {3, 3}}
	got, err := Select(pool, 2, 5, fixedRandom{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Winners, []int64{1, 2}) || !reflect.DeepEqual(got.Reserve, []int64{3}) {
		t.Fatal(got)
	}
	got, err = Select(nil, 5, 5, fixedRandom{})
	if err != nil || len(got.Winners) != 0 || len(got.Reserve) != 0 {
		t.Fatal(got, err)
	}
	for _, pool := range [][]Candidate{{{1, 0}}, {{1, 0}, {2, 1}}, {{0, 1}}, {{1, -1}}, {{1, 1}, {1, 2}}, {{1, int(^uint(0) >> 1)}, {2, 1}}} {
		_, err := Select(pool, 1, 0, fixedRandom{})
		faultCode(t, err, Invalid)
	}
	for _, counts := range [][2]int{{0, 0}, {1, -1}} {
		_, err := Select(nil, counts[0], counts[1], fixedRandom{})
		faultCode(t, err, Invalid)
	}
}

type brokenEntropy struct{}

func (brokenEntropy) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestEntropyFailureIsReturned(t *testing.T) {
	original := cryptorand.Reader
	cryptorand.Reader = brokenEntropy{}
	t.Cleanup(func() { cryptorand.Reader = original })
	if _, err := (CryptoRandom{}).Intn(7); err == nil {
		t.Fatal("entropy failure was hidden")
	}
	f := &Fault{Code: Invalid, Message: "Invalid request."}
	if f.Error() != "Invalid request." {
		t.Fatal("domain fault lost its technical message")
	}
}

func TestFaultSeparatesTechnicalAndBotMessages(t *testing.T) {
	err := UserFail(NotSubscribed, "Participant is not subscribed.", "Подпишитесь на канал партнёра.")
	wrapped := fmt.Errorf("register participant: %w", err)
	if wrapped.Error() != "register participant: Participant is not subscribed." {
		t.Fatalf("unexpected wrapped technical error: %v", wrapped)
	}
	fault, ok := FaultOf(wrapped)
	if !ok || fault.UserMessage != "Подпишитесь на канал партнёра." {
		t.Fatalf("wrapped error lost its bot message: %+v", fault)
	}
	data, err := json.Marshal(fault)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"code":"not_subscribed","message":"Participant is not subscribed."}` {
		t.Fatalf("API fault leaked bot text or changed its contract: %s", data)
	}
	plain, ok := FaultOf(Fail(Invalid, "Invalid setup."))
	if !ok || plain.UserMessage != "" || plain.Error() != "Invalid setup." {
		t.Fatalf("technical-only fault contains a bot message: %+v", plain)
	}
	input := "секретный ввод\nforged log entry"
	invalid := ValidateCommand(Command{UserID: 1, CampaignID: "campaign", Action: input})
	if invalid == nil || invalid.Error() != "Unknown command action." {
		t.Fatalf("invalid action leaked arbitrary input: %v", invalid)
	}
}

func TestValidateCommand(t *testing.T) {
	for _, action := range []string{"start", "promo", "participate", "check", "chances", "help", "rules", "claim", "claim_begin", "claim_name"} {
		if err := ValidateCommand(Command{UserID: 1, CampaignID: "c", Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range []Command{{UserID: 0, CampaignID: "c", Action: "start"}, {UserID: 1, Action: "start"}, {UserID: 1, CampaignID: "c", Action: "bad"}} {
		faultCode(t, ValidateCommand(cmd), Invalid)
	}
	if _, ok := FaultOf(errors.New("db error")); ok {
		t.Fatal("unexpected domain fault")
	}
}

func TestCryptoRandomRange(t *testing.T) {
	for range 20 {
		n, err := (CryptoRandom{}).Intn(7)
		if err != nil || n < 0 || n >= 7 {
			t.Fatalf("%d %v", n, err)
		}
	}
}

func FuzzActivityIdempotence(f *testing.F) {
	f.Add([]byte{1, 1, 2, 3, 10, 5})
	f.Fuzz(func(t *testing.T, actions []byte) {
		var active []Activity
		for _, v := range actions {
			n := int(v%10) + 1
			day := Day{ID: fmt.Sprint(n), Number: n}
			grant, err := GrantChance(active, day)
			if err != nil {
				t.Fatal(err)
			}
			if grant {
				active = append(active, Activity{DayID: day.ID})
			}
			if len(active) > 10 {
				t.Fatal("chance limit exceeded")
			}
			again, err := GrantChance(active, day)
			if err != nil || again {
				t.Fatal("action is not idempotent")
			}
		}
	})
}

func TestCommandSourceTimeValidation(t *testing.T) {
	for _, occurredAt := range []time.Time{time.Time{}, time.UnixMilli(1), time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)} {
		if err := ValidateCommand(Command{CampaignID: "campaign", UserID: 42, Action: "promo", OccurredAt: occurredAt}); err != nil {
			t.Fatalf("valid or legacy source time rejected: %s %v", occurredAt, err)
		}
	}
	for _, occurredAt := range []time.Time{time.UnixMilli(0), time.Unix(-1, 0), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		faultCode(t, ValidateCommand(Command{CampaignID: "campaign", UserID: 42, Action: "promo", OccurredAt: occurredAt}), Invalid)
	}
}
