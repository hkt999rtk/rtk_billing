package settlementcollector

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/usagecheckpoint"
)

type Persistence interface {
	ListCloudPreflightScopes(context.Context, string, int) ([]paymentstore.CloudPreflightScope, error)
	CaptureCloudPreflightState(context.Context, paymentstore.CloudPreflightScope) (paymentstore.CloudPreflightState, error)
	ReconcileSettlementEvidence(context.Context, paymentstore.ReconcileSettlementInput) (paymentstore.ReconciledSettlementEvidence, error)
	RecordCloudPreflightEvidence(context.Context, paymentstore.RecordCloudPreflightInput) error
	ListHandoffCollectorTargets(context.Context, int) ([]paymentstore.HandoffCollectorTarget, error)
	CaptureHandoffSettlementState(context.Context, paymentstore.HandoffScope) (paymentstore.SettlementState, error)
	RecordHandoffSettlement(context.Context, paymentstore.RecordSettlementInput) (paymentstore.HandoffSettlementStatus, error)
}

type Producer interface {
	Checkpoint(context.Context, usagecheckpoint.Scope) (usagecheckpoint.Evidence, error)
}

type Report struct {
	PreflightRecorded int
	HandoffRecorded   int
	Failed            int
}

type Service struct {
	store    Persistence
	producer Producer
	batch    int
	cursor   string
	newID    func() (string, error)
}

func New(store Persistence, producer Producer, batch int) (*Service, error) {
	if store == nil || producer == nil || batch < 1 || batch > 500 {
		return nil, errors.New("invalid settlement collector configuration")
	}
	return &Service{store: store, producer: producer, batch: batch, newID: randomUUID}, nil
}

func (s *Service) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 || interval > time.Minute {
		return errors.New("invalid settlement collector interval")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := s.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("settlement collector pass: preflight=%d handoff=%d failed=%d error=%v", report.PreflightRecorded, report.HandoffRecorded, report.Failed, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) RunOnce(ctx context.Context) (Report, error) {
	var report Report
	var failures []error
	scopes, err := s.store.ListCloudPreflightScopes(ctx, s.cursor, s.batch)
	if err != nil {
		return report, err
	}
	if len(scopes) == 0 && s.cursor != "" {
		s.cursor = ""
		scopes, err = s.store.ListCloudPreflightScopes(ctx, "", s.batch)
		if err != nil {
			return report, err
		}
	}
	for _, scope := range scopes {
		if err := s.collectPreflight(ctx, scope); err != nil {
			report.Failed++
			failures = append(failures, fmt.Errorf("cloud %s: %w", scope.OrganizationID, err))
			continue
		}
		report.PreflightRecorded++
	}
	if len(scopes) == s.batch {
		s.cursor = scopes[len(scopes)-1].OrganizationID
	} else {
		s.cursor = ""
	}

	targets, err := s.store.ListHandoffCollectorTargets(ctx, s.batch)
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, target := range targets {
			if err := s.collectHandoff(ctx, target); err != nil {
				report.Failed++
				failures = append(failures, fmt.Errorf("handoff %s: %w", target.Scope.OperationID, err))
				continue
			}
			report.HandoffRecorded++
		}
	}
	return report, errors.Join(failures...)
}

func (s *Service) collectPreflight(ctx context.Context, scope paymentstore.CloudPreflightScope) error {
	state, err := s.store.CaptureCloudPreflightState(ctx, scope)
	if err != nil {
		return err
	}
	producer, err := s.producer.Checkpoint(ctx, usagecheckpoint.Scope{CloudID: scope.OrganizationID, OwnerUserID: scope.OwnerUserID,
		OwnershipVersion: scope.OwnershipVersion, CoveredThrough: state.ObservedAt})
	if err != nil {
		return err
	}
	reconciled, err := s.store.ReconcileSettlementEvidence(ctx, paymentstore.ReconcileSettlementInput{OrganizationID: scope.OrganizationID,
		State: state.SettlementState, CoveredThrough: state.ObservedAt, ProducerCheckpointSHA256: producer.CheckpointSHA256})
	if err != nil {
		return err
	}
	receiptID, err := s.newID()
	if err != nil {
		return err
	}
	state.Financial = reconciled.Financial
	expires := producer.ExpiresAt
	if maximum := state.ObservedAt.Add(5 * time.Minute); expires.After(maximum) {
		expires = maximum
	}
	return s.store.RecordCloudPreflightEvidence(ctx, paymentstore.RecordCloudPreflightInput{State: state, ReceiptID: receiptID,
		UsageCheckpointSHA256: reconciled.UsageCheckpointSHA256, InvoiceCheckpointSHA256: reconciled.InvoiceCheckpointSHA256,
		ProviderCheckpointSHA256: reconciled.ProviderCheckpointSHA256, ExpiresAt: expires})
}

func (s *Service) collectHandoff(ctx context.Context, target paymentstore.HandoffCollectorTarget) error {
	state, err := s.store.CaptureHandoffSettlementState(ctx, target.Scope)
	if err != nil {
		return err
	}
	producer, err := s.producer.Checkpoint(ctx, usagecheckpoint.Scope{CloudID: target.Scope.OrganizationID, OwnerUserID: target.SourceUserID,
		OwnershipVersion: target.Scope.OwnershipVersion, CoveredThrough: target.Cutoff})
	if err != nil {
		return err
	}
	reconciled, err := s.store.ReconcileSettlementEvidence(ctx, paymentstore.ReconcileSettlementInput{OrganizationID: target.Scope.OrganizationID,
		State: state, CoveredThrough: target.Cutoff, ProducerCheckpointSHA256: producer.CheckpointSHA256})
	if err != nil {
		return err
	}
	receiptID, err := s.newID()
	if err != nil {
		return err
	}
	_, err = s.store.RecordHandoffSettlement(ctx, paymentstore.RecordSettlementInput{Scope: target.Scope, ReceiptID: receiptID, StateSHA256: state.SHA256,
		UsageCheckpointSHA256: reconciled.UsageCheckpointSHA256, InvoiceCheckpointSHA256: reconciled.InvoiceCheckpointSHA256,
		ProviderCheckpointSHA256: reconciled.ProviderCheckpointSHA256, Financial: reconciled.Financial})
	return err
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
