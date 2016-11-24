package endpoint

import (
	"fmt"
	"math/big"
	"net/http"

	"goji.io/pat"

	"github.com/spolu/settle/lib/db"
	"github.com/spolu/settle/lib/env"
	"github.com/spolu/settle/lib/errors"
	"github.com/spolu/settle/lib/format"
	"github.com/spolu/settle/lib/ptr"
	"github.com/spolu/settle/lib/svc"
	"github.com/spolu/settle/mint"
	"github.com/spolu/settle/mint/lib/authentication"
	"github.com/spolu/settle/mint/model"
)

const (
	// EndPtCreateOperation creates a new operation.
	EndPtCreateOperation EndPtName = "CreateOperation"
)

func init() {
	registrar[EndPtCreateOperation] = NewCreateOperation
}

// CreateOperation creates a new operation that moves asset from a source
// balance to a destination balance:
// - no `source` specified: issuance.
// - no `destination` specified: annihilation.
// - both specified: transfer from a balance to another.
// Only the asset creator can create operation on an asset. To transfer money,
// users owning an asset whould use transactions.
type CreateOperation struct {
	Client *mint.Client

	Owner       string
	Asset       mint.AssetResource
	Amount      big.Int
	Source      *string
	Destination *string
}

// NewCreateOperation constructs and initialiezes the endpoint.
func NewCreateOperation(
	r *http.Request,
) (Endpoint, error) {
	ctx := r.Context()

	client := &mint.Client{}
	err := client.Init(ctx)
	if err != nil {
		return nil, errors.Trace(err) // 500
	}

	return &CreateOperation{
		Client: client,
	}, nil
}

// Validate validates the input parameters.
func (e *CreateOperation) Validate(
	r *http.Request,
) error {
	ctx := r.Context()

	e.Owner = fmt.Sprintf("%s@%s",
		authentication.Get(ctx).User.Username,
		env.Get(ctx).Config[mint.EnvCfgMintHost])

	// Validate asset.
	a, err := mint.AssetResourceFromName(ctx, pat.Param(r, "asset"))
	if err != nil {
		return errors.Trace(errors.NewUserErrorf(err,
			400, "asset_invalid",
			"The asset name you provided is invalid: %s.",
			pat.Param(r, "asset"),
		))
	}
	e.Asset = *a

	// Validate that the issuer is attempting to create the operation.
	if e.Owner != a.Owner {
		return errors.Trace(errors.NewUserErrorf(nil,
			400, "operation_not_authorized",
			"You can only create operations for assets created by the "+
				"account you are currently authenticated with: %s. This "+
				"operation's asset was created by: %s. If you own %s, "+
				"and wish to transfer some of it, you should create a "+
				"transaction from your mint instead.",
			e.Owner, a.Owner, a.Name,
		))
	}

	// Validate amount.
	amount, err := ValidateAmount(ctx, r.PostFormValue("amount"))
	if err != nil {
		return errors.Trace(err)
	}
	e.Amount = *amount

	// Validate source.
	var srcAddress *string
	if r.PostFormValue("source") != "" {
		addr, err := mint.NormalizedAddress(ctx, r.PostFormValue("source"))
		if err != nil {
			return errors.Trace(errors.NewUserErrorf(err,
				400, "source_invalid",
				"The source address you provided is invalid: %s.",
				*srcAddress,
			))
		}
		srcAddress = &addr
	}
	e.Source = srcAddress

	// Validate destination.
	var dstAddress *string
	if r.PostFormValue("destination") != "" {
		addr, err := mint.NormalizedAddress(ctx, r.PostFormValue("destination"))
		if err != nil {
			return errors.Trace(errors.NewUserErrorf(err,
				400, "destination_invalid",
				"The destination address you provided is invalid: %s.",
				*dstAddress,
			))
		}
		dstAddress = &addr
	}
	e.Destination = dstAddress

	if srcAddress == nil && dstAddress == nil {
		return errors.Trace(errors.NewUserErrorf(err,
			400, "operation_invalid",
			"The operation has no source and no destination. You must "+
				"specify at least one of them (no source: issuance; no "+
				"destination: annihilation; source and destination: "+
				"transfer).",
		))
	}

	return nil
}

// Execute executes the endpoint.
func (e *CreateOperation) Execute(
	r *http.Request,
) (*int, *svc.Resp, error) {
	ctx := r.Context()

	ctx = db.Begin(ctx)
	defer db.LoggedRollback(ctx)

	asset, err := model.LoadAssetByOwnerCodeScale(ctx,
		e.Owner, e.Asset.Code, e.Asset.Scale)
	if err != nil {
		return nil, nil, errors.Trace(err) // 500
	} else if asset == nil {
		return nil, nil, errors.Trace(errors.NewUserErrorf(nil,
			400, "asset_not_found",
			"The asset you are trying to operate does not exist: %s. Try "+
				"creating it first.",
			e.Asset.Name,
		))
	}
	assetName := fmt.Sprintf(
		"%s[%s.%d]",
		asset.Owner, asset.Code, asset.Scale)

	balances := []*model.Balance{}

	var srcBalance *model.Balance
	if e.Source != nil && e.Asset.Owner != *e.Source {
		srcBalance, err = model.LoadBalanceByAssetHolder(ctx,
			assetName, *e.Source)
		if err != nil {
			return nil, nil, errors.Trace(err) // 500
		} else if srcBalance == nil {
			return nil, nil, errors.Trace(errors.NewUserErrorf(nil,
				400, "source_invalid",
				"The source address you provided has no existing balance: %s.",
				*e.Source,
			))
		}
		balances = append(balances, srcBalance)
	}

	var dstBalance *model.Balance
	if e.Destination != nil && e.Asset.Owner != *e.Destination {
		dstBalance, err = model.LoadOrCreateBalanceByAssetHolder(ctx,
			authentication.Get(ctx).User.Token,
			e.Owner,
			assetName, *e.Destination)
		if err != nil {
			return nil, nil, errors.Trace(err) // 500
		}

		balances = append(balances, dstBalance)
	}

	operation, err := model.CreateCanonicalOperation(ctx,
		authentication.Get(ctx).User.Token,
		e.Owner,
		assetName, e.Source, e.Destination, model.Amount(e.Amount))
	if err != nil {
		return nil, nil, errors.Trace(err) // 500
	}

	if dstBalance != nil {
		(*big.Int)(&dstBalance.Value).Add(
			(*big.Int)(&dstBalance.Value), (*big.Int)(&operation.Amount))

		// Checks if the dstBalance is positive and not overflown.
		b := (*big.Int)(&dstBalance.Value)
		if new(big.Int).Abs(b).Cmp(model.MaxAssetAmount) >= 0 ||
			b.Cmp(new(big.Int)) < 0 {
			return nil, nil, errors.Trace(errors.NewUserErrorf(err,
				400, "amount_invalid",
				"The resulting destination balance is invalid: %s. The "+
					"balance must be an integer between 0 and 2^128.",
				b.String(),
			))
		}

		err = dstBalance.Save(ctx)
		if err != nil {
			return nil, nil, errors.Trace(err) // 500
		}
	}

	if srcBalance != nil {
		(*big.Int)(&srcBalance.Value).Sub(
			(*big.Int)(&srcBalance.Value), (*big.Int)(&operation.Amount))

		// Checks if the srcBalance is positive and not overflown.
		b := (*big.Int)(&srcBalance.Value)
		if new(big.Int).Abs(b).Cmp(model.MaxAssetAmount) >= 0 ||
			b.Cmp(new(big.Int)) < 0 {
			return nil, nil, errors.Trace(errors.NewUserErrorf(nil,
				400, "amount_invalid",
				"The resulting source balance is invalid: %s. The "+
					"balance must be an integer between 0 and 2^128.",
				b.String(),
			))
		}

		err = srcBalance.Save(ctx)
		if err != nil {
			return nil, nil, errors.Trace(err) // 500
		}
	}

	db.Commit(ctx)

	// TODO(stan): propagation

	return ptr.Int(http.StatusCreated), &svc.Resp{
		"operation": format.JSONPtr(mint.NewOperationResource(ctx,
			operation, asset)),
	}, nil
}
