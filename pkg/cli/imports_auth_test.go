package cli

import (
	"testing"

	"github.com/getmockd/mockd/pkg/config"
	"github.com/getmockd/mockd/pkg/mock"
)

func extendAuthCollection(auth *mock.HTTPAuth) *config.MockCollection {
	return &config.MockCollection{
		Mocks: []*mock.Mock{{
			OperationID: "UpdateItems",
			Type:        mock.TypeHTTP,
			HTTP:        &mock.HTTPSpec{Matcher: &mock.HTTPMatcher{Method: "PATCH", Path: "/venues/{venueId}/items"}},
		}},
		Tables: []*config.TableConfig{{Name: "menus", IDField: "id"}},
		Extend: []*config.ExtendBinding{{Mock: "UpdateItems", Table: "menus", Action: "custom", Operation: "UpdateMenuItems", Auth: auth}},
	}
}

func TestProcessTablesAndExtend_AuthIsAppliedToBoundMock(t *testing.T) {
	auth := &mock.HTTPAuth{Bearer: &mock.BearerAuth{Provider: "wolt-auth",
		Claims: []mock.ClaimRequirement{{Claim: "integration.venue_id", EqualsPathParam: "venueId"}}}}
	col := extendAuthCollection(auth)
	if err := processTablesAndExtend(col); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if col.Mocks[0].HTTP.Auth != auth {
		t.Fatal("extend auth must be set on the bound mock")
	}
}

func TestProcessTablesAndExtend_InvalidAuthRejected(t *testing.T) {
	col := extendAuthCollection(&mock.HTTPAuth{Bearer: &mock.BearerAuth{}})
	if err := processTablesAndExtend(col); err == nil {
		t.Fatal("an extend binding with an invalid auth guard must be rejected")
	}
}

func TestProcessTablesAndExtend_NoAuthLeavesMockUnguarded(t *testing.T) {
	col := extendAuthCollection(nil)
	if err := processTablesAndExtend(col); err != nil {
		t.Fatal(err)
	}
	if col.Mocks[0].HTTP.Auth != nil {
		t.Fatal("no auth declared, none expected")
	}
}
