package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/params"
	"go.opencensus.io/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Validator interface defines the primary methods of a validator client.
type Validator interface {
	Done()
	WaitForChainStart(ctx context.Context) error
	WaitForSync(ctx context.Context) error
	WaitForActivation(ctx context.Context) error
	SlasherReady(ctx context.Context) error
	CanonicalHeadSlot(ctx context.Context) (uint64, error)
	NextSlot() <-chan uint64
	SlotDeadline(slot uint64) time.Time
	LogValidatorGainsAndLosses(ctx context.Context, slot uint64) error
	UpdateDuties(ctx context.Context, slot uint64) error
	UpdateProtections(ctx context.Context, slot uint64) error
	RolesAt(ctx context.Context, slot uint64) (map[[48]byte][]ValidatorRole, error) // validator pubKey -> roles
	SubmitAttestation(ctx context.Context, slot uint64, pubKey [48]byte)
	ProposeBlock(ctx context.Context, slot uint64, pubKey [48]byte)
	SubmitAggregateAndProof(ctx context.Context, slot uint64, pubKey [48]byte)
	LogAttestationsSubmitted()
	SaveProtections(ctx context.Context) error
	ResetAttesterProtectionData()
	UpdateDomainDataCaches(ctx context.Context, slot uint64)
	WaitForWalletInitialization(ctx context.Context) error
	AllValidatorsAreExited(ctx context.Context) (bool, error)
}

// Run the main validator routine. This routine exits if the context is
// canceled.
//
// Order of operations:
// 1 - Initialize validator data
// 2 - Wait for validator activation
// 3 - Wait for the next slot start
// 4 - Update assignments
// 5 - Determine role at current slot
// 6 - Perform assigned role, if any
func run(ctx context.Context, v Validator) {
	cleanup := v.Done
	defer cleanup()
	if err := v.WaitForWalletInitialization(ctx); err != nil {
		// log.Fatalf will prevent defer from being called
		cleanup()
		log.Fatalf("Wallet is not ready: %v", err)
	}
	if featureconfig.Get().SlasherProtection {
		if err := v.SlasherReady(ctx); err != nil {
			log.Fatalf("Slasher is not ready: %v", err)
		}
	}
	if err := v.WaitForChainStart(ctx); err != nil {
		log.Fatalf("Could not determine if beacon chain started: %v", err)
	}
	if err := v.WaitForSync(ctx); err != nil {
		log.Fatalf("Could not determine if beacon node synced: %v", err)
	}
	if err := v.WaitForActivation(ctx); err != nil {
		log.Fatalf("Could not wait for validator activation: %v", err)
	}
	headSlot, err := v.CanonicalHeadSlot(ctx)
	if err != nil {
		log.Fatalf("Could not get current canonical head slot: %v", err)
	}
	if err := v.UpdateDuties(ctx, headSlot); err != nil {
		handleAssignmentError(err, headSlot)
	}

	for {
		ctx, span := trace.StartSpan(ctx, "validator.processSlot")

		select {
		case <-ctx.Done():
			log.Info("Context canceled, stopping validator")
			span.End()
			return // Exit if context is canceled.
		case slot := <-v.NextSlot():
			span.AddAttributes(trace.Int64Attribute("slot", int64(slot)))

			allExited, err := v.AllValidatorsAreExited(ctx)
			if err != nil {
				log.WithError(err).Error("Could not check if validators are exited")
			}
			if allExited {
				log.Info("All validators are exited, no more work to perform...")
				continue
			}

			deadline := v.SlotDeadline(slot)
			slotCtx, cancel := context.WithDeadline(ctx, deadline)

			log := log.WithField("slot", slot)
			log.WithField("deadline", deadline).Debug("Set deadline for proposals and attestations")

			// Keep trying to update assignments if they are nil or if we are past an
			// epoch transition in the beacon node's state.
			if err := v.UpdateDuties(ctx, slot); err != nil {
				handleAssignmentError(err, slot)
				cancel()
				span.End()
				continue
			}

			if err := v.UpdateProtections(ctx, slot); err != nil {
				log.WithError(err).Error("Could not update validator protection")
				span.End()
				continue
			}

			// Start fetching domain data for the next epoch.
			if helpers.IsEpochEnd(slot) {
				go v.UpdateDomainDataCaches(ctx, slot+1)
			}

			var wg sync.WaitGroup

			allRoles, err := v.RolesAt(ctx, slot)
			if err != nil {
				log.WithError(err).Error("Could not get validator roles")
				span.End()
				continue
			}
			for pubKey, roles := range allRoles {
				wg.Add(len(roles))
				for _, role := range roles {
					go func(role ValidatorRole, pubKey [48]byte) {
						defer wg.Done()
						switch role {
						case roleAttester:
							v.SubmitAttestation(slotCtx, slot, pubKey)
						case roleProposer:
							v.ProposeBlock(slotCtx, slot, pubKey)
						case roleAggregator:
							v.SubmitAggregateAndProof(slotCtx, slot, pubKey)
						case roleUnknown:
							log.WithField("pubKey", fmt.Sprintf("%#x", bytesutil.Trunc(pubKey[:]))).Trace("No active roles, doing nothing")
						default:
							log.Warnf("Unhandled role %v", role)
						}
					}(role, pubKey)
				}
			}
			// Wait for all processes to complete, then report span complete.
			go func() {
				wg.Wait()
				v.ResetAttesterProtectionData()
				v.LogAttestationsSubmitted()
				// Log this client performance in the previous epoch
				if err := v.LogValidatorGainsAndLosses(slotCtx, slot); err != nil {
					log.WithError(err).Error("Could not report validator's rewards/penalties")
				}
				span.End()
			}()
		}
	}
}

func handleAssignmentError(err error, slot uint64) {
	if errCode, ok := status.FromError(err); ok && errCode.Code() == codes.NotFound {
		log.WithField(
			"epoch", slot/params.BeaconConfig().SlotsPerEpoch,
		).Warn("Validator not yet assigned to epoch")
	} else {
		log.WithField("error", err).Error("Failed to update assignments")
	}
}
