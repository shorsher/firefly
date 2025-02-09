// Copyright © 2022 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operations

import (
	"context"
	"fmt"

	"github.com/hyperledger/firefly-common/pkg/fftypes"
	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-common/pkg/log"
	"github.com/hyperledger/firefly/internal/coremsgs"
	"github.com/hyperledger/firefly/internal/txcommon"
	"github.com/hyperledger/firefly/pkg/core"
	"github.com/hyperledger/firefly/pkg/database"
)

type OperationHandler interface {
	core.Named
	PrepareOperation(ctx context.Context, op *core.Operation) (*core.PreparedOperation, error)
	RunOperation(ctx context.Context, op *core.PreparedOperation) (outputs fftypes.JSONObject, complete bool, err error)
	OnOperationUpdate(ctx context.Context, op *core.Operation, update *core.OperationUpdate) error
}

type Manager interface {
	RegisterHandler(ctx context.Context, handler OperationHandler, ops []core.OpType)
	PrepareOperation(ctx context.Context, op *core.Operation) (*core.PreparedOperation, error)
	RunOperation(ctx context.Context, op *core.PreparedOperation, options ...RunOperationOption) (fftypes.JSONObject, error)
	RetryOperation(ctx context.Context, opID *fftypes.UUID) (*core.Operation, error)
	AddOrReuseOperation(ctx context.Context, op *core.Operation) error
	SubmitOperationUpdate(update *core.OperationUpdate)
	ResolveOperationByID(ctx context.Context, opID *fftypes.UUID, op *core.OperationUpdateDTO) error
	Start() error
	WaitStop()
}

type RunOperationOption int

const (
	RemainPendingOnFailure RunOperationOption = iota
)

type operationsManager struct {
	ctx       context.Context
	namespace string
	database  database.Plugin
	handlers  map[core.OpType]OperationHandler
	updater   *operationUpdater
}

func NewOperationsManager(ctx context.Context, ns string, di database.Plugin, txHelper txcommon.Helper) (Manager, error) {
	if di == nil || txHelper == nil {
		return nil, i18n.NewError(ctx, coremsgs.MsgInitializationNilDepError, "OperationsManager")
	}
	om := &operationsManager{
		ctx:       ctx,
		namespace: ns,
		database:  di,
		handlers:  make(map[core.OpType]OperationHandler),
	}
	updater := newOperationUpdater(ctx, om, di, txHelper)
	om.updater = updater
	return om, nil
}

func (om *operationsManager) RegisterHandler(ctx context.Context, handler OperationHandler, ops []core.OpType) {
	for _, opType := range ops {
		log.L(ctx).Debugf("OpType=%s registered to handler %s", opType, handler.Name())
		om.handlers[opType] = handler
	}
}

func (om *operationsManager) PrepareOperation(ctx context.Context, op *core.Operation) (*core.PreparedOperation, error) {
	handler, ok := om.handlers[op.Type]
	if !ok {
		return nil, i18n.NewError(ctx, coremsgs.MsgOperationNotSupported, op.Type)
	}
	return handler.PrepareOperation(ctx, op)
}

func (om *operationsManager) RunOperation(ctx context.Context, op *core.PreparedOperation, options ...RunOperationOption) (fftypes.JSONObject, error) {
	failState := core.OpStatusFailed
	for _, o := range options {
		if o == RemainPendingOnFailure {
			failState = core.OpStatusPending
		}
	}

	handler, ok := om.handlers[op.Type]
	if !ok {
		return nil, i18n.NewError(ctx, coremsgs.MsgOperationNotSupported, op.Type)
	}
	log.L(ctx).Infof("Executing %s operation %s via handler %s", op.Type, op.ID, handler.Name())
	log.L(ctx).Tracef("Operation detail: %+v", op)
	outputs, complete, err := handler.RunOperation(ctx, op)
	if err != nil {
		om.writeOperationFailure(ctx, op.ID, outputs, err, failState)
		return nil, err
	} else if complete {
		om.writeOperationSuccess(ctx, op.ID, outputs)
	}
	return outputs, nil
}

func (om *operationsManager) findLatestRetry(ctx context.Context, opID *fftypes.UUID) (op *core.Operation, err error) {
	op, err = om.database.GetOperationByID(ctx, om.namespace, opID)
	if err != nil {
		return nil, err
	}
	if op.Retry == nil {
		return op, nil
	}
	return om.findLatestRetry(ctx, op.Retry)
}

func (om *operationsManager) RetryOperation(ctx context.Context, opID *fftypes.UUID) (op *core.Operation, err error) {
	var po *core.PreparedOperation
	err = om.database.RunAsGroup(ctx, func(ctx context.Context) error {
		op, err = om.findLatestRetry(ctx, opID)
		if err != nil {
			return err
		}

		// Create a copy of the operation with a new ID
		op.ID = fftypes.NewUUID()
		op.Status = core.OpStatusPending
		op.Error = ""
		op.Output = nil
		op.Created = fftypes.Now()
		op.Updated = op.Created
		if err = om.database.InsertOperation(ctx, op); err != nil {
			return err
		}

		// Update the old operation to point to the new one
		update := database.OperationQueryFactory.NewUpdate(ctx).Set("retry", op.ID)
		if err = om.database.UpdateOperation(ctx, om.namespace, opID, update); err != nil {
			return err
		}

		po, err = om.PrepareOperation(ctx, op)
		return err
	})
	if err != nil {
		return nil, err
	}

	_, err = om.RunOperation(ctx, po)
	return op, err
}

func (om *operationsManager) writeOperationSuccess(ctx context.Context, opID *fftypes.UUID, outputs fftypes.JSONObject) {
	emptyString := ""
	if err := om.database.ResolveOperation(ctx, om.namespace, opID, core.OpStatusSucceeded, &emptyString, outputs); err != nil {
		log.L(ctx).Errorf("Failed to update operation %s: %s", opID, err)
	}
}

func (om *operationsManager) writeOperationFailure(ctx context.Context, opID *fftypes.UUID, outputs fftypes.JSONObject, err error, newStatus core.OpStatus) {
	errMsg := err.Error()
	if err := om.database.ResolveOperation(ctx, om.namespace, opID, newStatus, &errMsg, outputs); err != nil {
		log.L(ctx).Errorf("Failed to update operation %s: %s", opID, err)
	}
}

func (om *operationsManager) ResolveOperationByID(ctx context.Context, opID *fftypes.UUID, op *core.OperationUpdateDTO) error {
	return om.database.ResolveOperation(ctx, om.namespace, opID, op.Status, op.Error, op.Output)
}

func (om *operationsManager) SubmitOperationUpdate(update *core.OperationUpdate) {
	errString := ""
	if update.ErrorMessage != "" {
		errString = fmt.Sprintf(" error=%s", update.ErrorMessage)
	}
	log.L(om.ctx).Debugf("%s updating operation %s status=%s%s", update.Plugin, update.NamespacedOpID, update.Status, errString)
	om.updater.SubmitOperationUpdate(om.ctx, update)
}

func (om *operationsManager) Start() error {
	om.updater.start()
	return nil
}

func (om *operationsManager) WaitStop() {
	om.updater.close()
}
