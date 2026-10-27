package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/liquiditylab/liquiditylab/internal/api"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
)

func TestPaperSessionHistoryEndToEndIsRevisionBoundAndReadOnly(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	source := func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		if err := deliver(ctx, daemonSnapshot()); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	daemon := daemonNewTest(t, opt, dsn, source)
	ready := daemonWait(t, daemon, func(v rt.View) bool { return v.Account.Ready })
	route := "/v1/accounts/" + daemon.key.TenantID + "/" + daemon.key.AccountID

	plan := d.DefaultPlan()
	plan.BaseBalance, plan.QuoteBudget = d.Zero, d.MustDecimal("10")
	plan.Quantity, plan.MaxChildQuantity = d.MustDecimal("0.02"), d.MustDecimal("0.01")
	plan.LimitPrice, plan.Deadline = d.MustDecimal("102"), 10*d.Second
	createdResponse := daemonRequest(t, daemon.handler, http.MethodPost, route+"/commands", daemonTestToken, map[string]any{
		"id": "history-old-create", "kind": "create", "expected_revision": ready.Account.Revision, "expected_authority_revision": ready.AuthorityRevision(),
		"plan_id": "history-old-plan", "plan": plan, "policy": "scheduled",
	})
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("create historical plan: %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	var created struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil || created.Revision <= 0 {
		t.Fatalf("decode create revision: %+v err=%v body=%s", created, err, createdResponse.Body.String())
	}
	paused := daemonView(t, daemon)
	startedResponse := daemonRequest(t, daemon.handler, http.MethodPost, route+"/commands", daemonTestToken, map[string]any{
		"id": "history-old-start", "kind": "start", "expected_revision": paused.Account.Revision, "expected_authority_revision": paused.AuthorityRevision(),
	})
	if startedResponse.Code != http.StatusOK {
		t.Fatalf("start historical plan: %d %s", startedResponse.Code, startedResponse.Body.String())
	}
	filled := daemonWait(t, daemon, func(v rt.View) bool { return len(v.State.Fills) > 0 && v.State.Child == nil })
	if filled.State.Parent == nil || filled.State.Parent.ID != "history-old-plan" || filled.State.Parent.Status != "active" {
		t.Fatalf("automatic paper execution did not record an active plan fill: parent=%+v fills=%+v", filled.State.Parent, filled.State.Fills)
	}
	firstFill := filled.State.Fills[0]
	cancelledResponse := daemonRequest(t, daemon.handler, http.MethodPost, route+"/commands", daemonTestToken, map[string]any{
		"id": "history-old-cancel", "kind": "cancel", "expected_revision": filled.Account.Revision, "expected_authority_revision": filled.AuthorityRevision(),
	})
	if cancelledResponse.Code != http.StatusOK {
		t.Fatalf("cancel filled historical plan: %d %s", cancelledResponse.Code, cancelledResponse.Body.String())
	}
	cancelled := daemonWait(t, daemon, func(v rt.View) bool {
		return v.State.Parent != nil && v.State.Parent.ID == "history-old-plan" && v.State.Parent.Status == "cancelled"
	})
	if len(cancelled.State.Fills) == 0 || !cancelled.State.Fills[0].Equal(firstFill) {
		t.Fatal("known-final cancel lost the recorded simulated fill")
	}

	newPlan := plan
	newPlan.Quantity, newPlan.MaxChildQuantity = d.MustDecimal("0.01"), d.MustDecimal("0.01")
	newPlan.QuoteBudget = d.MustDecimal("5")
	newCreated := daemonRequest(t, daemon.handler, http.MethodPost, route+"/commands", daemonTestToken, map[string]any{
		"id": "history-new-create", "kind": "create", "expected_revision": cancelled.Account.Revision, "expected_authority_revision": cancelled.AuthorityRevision(),
		"plan_id": "history-new-plan", "plan": newPlan, "policy": "scheduled",
	})
	if newCreated.Code != http.StatusOK {
		t.Fatalf("create replacement paused plan: %d %s", newCreated.Code, newCreated.Body.String())
	}
	current := daemonView(t, daemon)
	if current.State.Parent == nil || current.State.Parent.ID != "history-new-plan" || current.State.Parent.Status != "paused" {
		t.Fatalf("replacement plan not paused: %+v", current.State.Parent)
	}
	readRevision := current.Account.Revision

	listResponse := daemonRequest(t, daemon.handler, http.MethodGet, route+"/sessions?limit=10", daemonTestToken, nil)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list historical sessions: %d %s", listResponse.Code, listResponse.Body.String())
	}
	var page struct {
		Entries      []api.SessionSummary `json:"entries"`
		AsOfRevision int64                `json:"as_of_revision"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.AsOfRevision != readRevision || len(page.Entries) != 2 || page.Entries[0].PlanID != "history-new-plan" || page.Entries[0].Status != "paused" || page.Entries[1].PlanID != "history-old-plan" || page.Entries[1].Status != "cancelled" {
		t.Fatalf("session list didn't reflect latest parent states: %+v", page)
	}

	oldPath := route + "/sessions/history-old-plan"
	oldResponse := daemonRequest(t, daemon.handler, http.MethodGet, oldPath, daemonTestToken, nil)
	if oldResponse.Code != http.StatusOK {
		t.Fatalf("read cancelled plan report: %d %s", oldResponse.Code, oldResponse.Body.String())
	}
	var report api.SessionReport
	if err := json.Unmarshal(oldResponse.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.PlanID != "history-old-plan" || report.Status != "cancelled" || !report.KnownFinal || report.Revision != cancelled.Account.Revision || len(report.Fills) != len(cancelled.State.Fills) || len(report.Fills) == 0 || !report.Fills[0].Equal(firstFill) {
		t.Fatalf("historical report lost exact fill or final status: %+v", report)
	}
	feeOK := report.Plan != nil
	if feeOK && cancelled.State.Fills[0].FeeCurrency == "base" {
		feeOK = report.Plan.FeesBase.Equal(cancelled.State.Fills[0].Fee)
	} else if feeOK {
		feeOK = report.Plan.FeesQuote.Equal(cancelled.State.Fills[0].Fee)
	}
	if !feeOK {
		t.Fatalf("historical report lost fee accounting: plan=%+v fill=%+v", report.Plan, cancelled.State.Fills[0])
	}
	for _, secret := range []string{"owner_id", "ownership_epoch", "last_observation", "pending_facts", "raw-snapshot", "features"} {
		if strings.Contains(oldResponse.Body.String(), secret) {
			t.Fatalf("historical report leaked %q: %s", secret, oldResponse.Body.String())
		}
	}

	createdRevision := strconv.FormatInt(created.Revision, 10)
	beforeFill := daemonRequest(t, daemon.handler, http.MethodGet, oldPath+"?as_of_revision="+createdRevision, daemonTestToken, nil)
	if beforeFill.Code != http.StatusOK {
		t.Fatalf("read report before fill: %d %s", beforeFill.Code, beforeFill.Body.String())
	}
	var earlier api.SessionReport
	if err := json.Unmarshal(beforeFill.Body.Bytes(), &earlier); err != nil {
		t.Fatal(err)
	}
	if earlier.AsOfRevision != created.Revision || len(earlier.Fills) != 0 || earlier.Status != "paused" {
		t.Fatalf("historical point before execution includes later fills or parent state: %+v", earlier)
	}

	readRevisionText := strconv.FormatInt(readRevision, 10)
	timeline := daemonRequest(t, daemon.handler, http.MethodGet, oldPath+"/timeline?as_of_revision="+readRevisionText+"&limit=200", daemonTestToken, nil)
	if timeline.Code != http.StatusOK || !strings.Contains(timeline.Body.String(), `"as_of_revision":`+readRevisionText) {
		t.Fatalf("session timeline wasn't pinned to requested account revision: %d %s", timeline.Code, timeline.Body.String())
	}
	unauthorized := daemonRequest(t, daemon.handler, http.MethodGet, "/v1/accounts/other-tenant/"+daemon.key.AccountID+"/sessions", daemonTestToken, nil)
	if unauthorized.Code != http.StatusNotFound {
		t.Fatalf("same token escaped session account scope: %d %s", unauthorized.Code, unauthorized.Body.String())
	}
	afterReads := daemonView(t, daemon)
	if afterReads.Account.Revision != readRevision {
		t.Fatalf("history reads mutated account revision: before=%d after=%d", readRevision, afterReads.Account.Revision)
	}
}
