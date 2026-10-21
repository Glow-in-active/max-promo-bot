package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"promo/internal/domain"
)

func TestMissingStorageResourcesReturnTechnicalFaults(t *testing.T) {
	for _, err := range []error{
		missing(pgx.ErrNoRows, "Campaign"),
		changed(pgconn.NewCommandTag("UPDATE 0"), nil, "Campaign"),
	} {
		fault, ok := domain.FaultOf(err)
		if !ok || fault.Code != domain.NotFound || fault.Message != "Campaign not found." || fault.UserMessage != "" {
			t.Fatalf("unexpected storage fault: %+v", fault)
		}
	}
	connectionError := errors.New("database connection unavailable")
	if missing(connectionError, "Campaign") != connectionError || changed(pgconn.CommandTag{}, connectionError, "Campaign") != connectionError {
		t.Fatal("storage helper replaced the underlying database failure")
	}
	if missing(nil, "Campaign") != nil || changed(pgconn.NewCommandTag("UPDATE 1"), nil, "Campaign") != nil {
		t.Fatal("successful storage operation returned an error")
	}
}
