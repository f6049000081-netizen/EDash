package ledger

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"superapp/backend/internal/money"
)

func TestValidate(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	src := uuid.New()
	base := func(ps ...Posting) Entry {
		return Entry{Kind: "test", SourceType: "test", SourceID: src, Postings: ps}
	}
	cases := []struct {
		name    string
		entry   Entry
		wantErr error
	}{
		{"balanced pair", base(
			Posting{a, Debit, 500, money.ETB},
			Posting{b, Credit, 500, money.ETB}), nil},
		{"three-way split", base(
			Posting{a, Debit, 18500, money.ETB},
			Posting{b, Credit, 18407, money.ETB},
			Posting{c, Credit, 93, money.ETB}), nil},
		{"off by one santim", base(
			Posting{a, Debit, 500, money.ETB},
			Posting{b, Credit, 499, money.ETB}), ErrUnbalanced},
		{"balanced in total but not per currency", base(
			Posting{a, Debit, 500, money.ETB},
			Posting{b, Credit, 500, money.USD}), ErrUnbalanced},
		{"single posting", base(Posting{a, Debit, 500, money.ETB}), ErrUnbalanced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.entry.Validate()
			if tc.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateRejectsBadAmounts(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	for _, amt := range []int64{0, -100, money.MaxAmountMinor + 1} {
		e := Entry{Kind: "t", SourceType: "t", SourceID: uuid.New(), Postings: []Posting{
			{a, Debit, amt, money.ETB}, {b, Credit, amt, money.ETB}}}
		if e.Validate() == nil {
			t.Errorf("amount %d should be rejected", amt)
		}
	}
}

func TestValidateNeedsSource(t *testing.T) {
	e := Entry{Kind: "t", SourceType: "t", Postings: []Posting{
		{uuid.New(), Debit, 1, money.ETB}, {uuid.New(), Credit, 1, money.ETB}}}
	if e.Validate() == nil {
		t.Fatal("entry without a source id should be rejected")
	}
}
