package api

import (
	"encoding/json"
	"strings"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func TestAuthorityRevisionDTOsAndCommandOptIn(t *testing.T) {
	h, authority, _ := setup(t)
	authority.view.State.AuthorityRevision = 4
	for _, path := range []string{pathA, "/v1/accounts", pathA + "/preview"} {
		method, body := "GET", ""
		if strings.HasSuffix(path, "/preview") {
			method = "POST"
			plan := d.DefaultPlan()
			body = bodyJSON(previewRequest{Plan: &plan})
		}
		response := request(h, method, path, tokenA, body)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"authority_revision":4`) || !strings.Contains(response.Body.String(), `"revision":7`) {
			t.Fatalf("%s omitted distinct revisions: status=%d body=%s", path, response.Code, response.Body)
		}
	}
	response := request(h, "POST", pathA+"/commands", tokenA, `{"id":"authority-command","expected_revision":7,"expected_authority_revision":4,"kind":"pause"}`)
	if response.Code != 200 || authority.command.ExpectedAuthorityRevision == nil || *authority.command.ExpectedAuthorityRevision != 4 || authority.command.ExpectedRevision != 7 {
		t.Fatalf("command did not forward authority token: %d %s command=%+v", response.Code, response.Body, authority.command)
	}
	for _, token := range []string{"-1", "4.5", `"4"`} {
		response = request(h, "POST", pathA+"/commands", tokenA, `{"id":"invalid-authority-command","expected_revision":7,"expected_authority_revision":`+token+`,"kind":"pause"}`)
		if response.Code != 400 {
			t.Fatalf("invalid authority token %s: status=%d body=%s", token, response.Code, response.Body)
		}
	}
	// Legacy in-memory views and pre-marker snapshots use the durable revision.
	authority.view.State.AuthorityRevision = 0
	response = request(h, "GET", pathA, tokenA, "")
	var account AccountDTO
	if err := json.Unmarshal(response.Body.Bytes(), &account); err != nil || account.AuthorityRevision != account.Revision || account.AuthorityRevision != 7 {
		t.Fatalf("legacy authority fallback=%+v err=%v", account, err)
	}
}
