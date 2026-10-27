package plugin

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

func TestPreflightMapsTheClassifiedError(t *testing.T) {
	r := newRig(t)
	r.start()
	if err := r.p.Preflight(context.Background(), r.job()); err != nil {
		t.Fatal(err)
	}
	r2 := newRig(t, fault("preflight-ineligible"))
	r2.start()
	var ine *provider.ErrIneligible
	if err := r2.p.Preflight(context.Background(), r2.job()); !errors.As(err, &ine) || ine.Why != "request too large" {
		t.Fatalf("got %v", err)
	}
}

func TestMethodsFirstStartThePlugin(t *testing.T) {
	r := newRig(t)
	if err := r.p.Preflight(context.Background(), r.job()); err != nil {
		t.Fatalf("Preflight without an explicit Start: %v", err)
	}
	if !r.exists("diag.json") {
		t.Error("the plugin was not spawned by the first call (A.3.1)")
	}
}

func TestTimeoutSendsCancelAndIsTransient(t *testing.T) {
	r := newRig(t, fault("hang:estimate"), fire("estimate", 300*time.Millisecond)) // never answers
	r.start()
	_, err := r.p.Estimate(context.Background(), r.job())
	var tr *provider.ErrTransient
	if !errors.As(err, &tr) || !strings.HasPrefix(tr.Why, "estimate timed out after ") {
		t.Fatalf("got %T %v", err, err)
	}
	r.await("cancel.log")
	if !strings.Contains(string(r.read("cancel.log")), `"id":`) {
		t.Errorf("the plugin got no cancel frame")
	}
	first := r.diag().Pid
	r.awaitGone(first) // the plugin ignored the cancel: bitc kills it after its grace (5 s)
	if err := r.p.Preflight(context.Background(), r.job()); err != nil {
		t.Fatalf("after a plugin ignored the cancel: %v", err)
	}
	if r.diag().Pid == first {
		t.Error("a plugin that ignored the cancel was not replaced")
	}
}

func TestRequestsAreMultiplexedByID(t *testing.T) {
	r := newRig(t, fault("hang:watch")) // the watch never answers
	r.start()
	h := provider.Handle{ID: "r-x", Name: "x"}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, err := r.p.Watch(ctx, h, newSink()); done <- err }()
	r.await("hanging.log") // the watch is in flight in the plugin
	if err := r.p.Teardown(context.Background(), "r-x"); err != nil {
		t.Fatal(err)
	}
	select { // Teardown answered while the watch is still open: it did not wait for it
	case err := <-done:
		t.Fatalf("the watch ended first: %v", err)
	default:
	}
	cancel()
	<-done
}

func TestOversizeLineIsAProtocolError(t *testing.T) {
	r := newRig(t, fault("owner-oversize"))
	r.start()
	_, err := r.p.Owner(context.Background(), "r-x")
	wantUnavailableLocal(t, err, "plugin protocol error: a line is longer than 1 MiB")
}

func TestOwnerTeardownListSpendAndStreams(t *testing.T) {
	t.Setenv("STUB_SPEND", "1") // hello announces caps.spend
	r := newRig(t, raw("spend_micros", int64(1_500_000)), raw("output", "stdout-bytes"))
	r.start()
	ctx := context.Background()
	j := r.job()
	h, err := r.p.Launch(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != "r-"+j.Name || h.Name != j.Name || h.ResultsURL != "http://127.0.0.1:1/v1/" {
		t.Fatalf("handle = %+v", h)
	}
	if o, err := r.p.Owner(ctx, h.ID); err != nil || o != j.Owner {
		t.Errorf("Owner = %q, %v", o, err)
	}
	list, err := r.p.List(ctx, []string{"bitc-"})
	if err != nil || len(list) != 1 || list[0].ID != h.ID || list[0].Name != j.Name {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if got := list[0]; got.SizeKey != "cpu8" || got.State != "running" || got.Created.IsZero() || got.Expires.IsZero() {
		t.Errorf("resource fields: %+v", got)
	}
	if s, err := r.p.Spend(ctx); err != nil || s != 1_500_000 {
		t.Errorf("Spend = %v, %v", s, err)
	}
	var w bytes.Buffer
	if err := r.p.Output(ctx, h, provider.Stdout, 0, &w); err != nil || w.String() != "stdout-bytes" {
		t.Errorf("Output = %q, %v", w.String(), err)
	}
	w.Reset()
	if err := r.p.Fetch(ctx, h, provider.ArtifactSpec{MaxCompressed: 1 << 20}, &w); err != nil || w.String() != "tarball" {
		t.Errorf("Fetch = %q, %v", w.String(), err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := r.p.Teardown(ctx, h.ID); err != nil {
			t.Fatalf("Teardown #%d: %v", i, err)
		}
	}
	if len(r.resources()) != 0 {
		t.Errorf("resources left: %v", r.resources())
	}
}

func TestSpendIsNotAskedWithoutTheCapability(t *testing.T) {
	r := newRig(t, raw("spend_micros", int64(9_000_000))) // the stub would answer, but hello says spend:false
	r.start()
	if s, err := r.p.Spend(context.Background()); err != nil || s != 0 {
		t.Errorf("Spend = %v, %v", s, err)
	}
}

func overCap(t *testing.T, err error, field string) {
	t.Helper()
	var tr *provider.ErrTransient
	want := "daily cap $5.00: plugin stub reported " + field + " out of range"
	if !errors.As(err, &tr) || tr.Why != want {
		t.Fatalf("got %T %v, want ErrTransient %q", err, err, want)
	}
}

func TestEstimateIsTheMaxOfHostPriceAndPluginFigure(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		plugin int64
		want   func(got config.Micros) bool
	}{
		{0, func(g config.Micros) bool { return g >= 595_000 && g <= 600_000 }},
		{900_000, func(g config.Micros) bool { return g == 900_000 }},
	} {
		r := newRig(t, raw("estimate_micros", c.plugin))
		r.start()
		j := r.job()
		j.Expiry = time.Now().Add(time.Hour)
		got, err := r.p.Estimate(ctx, j)
		if err != nil || !c.want(got) {
			t.Errorf("plugin figure %d: Estimate = %v, %v", c.plugin, got, err)
		}
	}
	for f, field := range map[string]string{"estimate-negative": "usd_micros", "estimate-float": "usd_micros"} {
		r := newRig(t, fault(f))
		r.start()
		_, err := r.p.Estimate(ctx, r.job())
		overCap(t, err, field)
	}
}

func TestEstimateOfAnUnknownSizePricesAtTheHighestHostSize(t *testing.T) {
	r := newRig(t)
	r.core.Sizes["gpu1"] = 2_400_000
	r.start()
	j := r.job()
	j.SizeKey, j.Expiry = "\x00unknown", time.Now().Add(time.Hour)
	got, err := r.p.Estimate(context.Background(), j)
	if err != nil || got < 2_390_000 || got > 2_400_000 {
		t.Errorf("Estimate = %v, %v", got, err)
	}
}

func TestListPricesEachResourceAtTheMaxOfHostAndPlugin(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, raw("price", 0.2)) // plugin says $0.20/h, the host price of cpu8 is $0.60
	r.core.Sizes["gpu1"] = 2_400_000
	r.start()
	j := r.job()
	if _, err := r.p.Launch(ctx, j); err != nil {
		t.Fatal(err)
	}
	list, err := r.p.List(ctx, []string{"bitc-"})
	if err != nil || len(list) != 1 || list[0].USDPerHour != 600_000 {
		t.Fatalf("host price is higher: %+v %v", list, err)
	}
	r2 := newRig(t, raw("price", 0.9))
	r2.start()
	if _, err := r2.p.Launch(ctx, r2.job()); err != nil {
		t.Fatal(err)
	}
	if list, err := r2.p.List(ctx, []string{"bitc-"}); err != nil || list[0].USDPerHour != 900_000 {
		t.Fatalf("plugin price is higher: %+v %v", list, err)
	}
	r3 := newRig(t, raw("price", 0.1))
	r3.core.Sizes["gpu1"] = 2_400_000
	r3.start()
	j3 := r3.job()
	j3.SizeKey = "weird"
	j3.Name += "-w"
	if _, err := r3.p.Launch(ctx, j3); err != nil {
		t.Fatal(err)
	}
	if list, err := r3.p.List(ctx, []string{"bitc-"}); err != nil || list[0].USDPerHour != 2_400_000 {
		t.Fatalf("an unknown size takes the highest host price: %+v %v", list, err)
	}
}

func TestListOutOfRangeAnswersAreOverTheCap(t *testing.T) {
	ctx := context.Background()
	for f, field := range map[string]string{"list-negative-price": "usd_per_hour", "list-foreign-name": "name", "list-no-id": "id"} {
		r := newRig(t, fault(f))
		r.start()
		if _, err := r.p.Launch(ctx, r.job()); err != nil {
			t.Fatal(err)
		}
		_, err := r.p.List(ctx, []string{"bitc-"})
		overCap(t, err, field)
	}
}

func TestSpendOutOfRangeIsOverTheCap(t *testing.T) {
	t.Setenv("STUB_SPEND", "1")
	r := newRig(t, fault("spend-negative"))
	r.start()
	_, err := r.p.Spend(context.Background())
	overCap(t, err, "usd_micros")
}

func TestEstimateOfAnUnknownSizeNeverAsksThePlugin(t *testing.T) {
	r := newRig(t, fault("estimate-negative")) // the plugin would answer out of range if it were asked
	r.start()
	j := r.job()
	j.SizeKey, j.Expiry = "\x00unknown", time.Now().Add(time.Hour)
	got, err := r.p.Estimate(context.Background(), j)
	if err != nil || got < 595_000 || got > 600_000 {
		t.Fatalf("Estimate = %v, %v: the rails' highest-price probe is answered from the host's sizes", got, err)
	}
}
