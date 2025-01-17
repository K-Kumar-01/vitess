/*
Copyright 2022 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package wrangler

import (
	"context"
	"sync"

	"vitess.io/vitess/go/vt/vtctl/workflow"

	"vitess.io/vitess/go/vt/log"

	vdiff2 "vitess.io/vitess/go/vt/vttablet/tabletmanager/vdiff"

	"vitess.io/vitess/go/vt/proto/tabletmanagerdata"
)

type VDiffOutput struct {
	mu        sync.Mutex
	Request   *tabletmanagerdata.VDiffRequest
	Responses map[string]*tabletmanagerdata.VDiffResponse
	Err       error
}

func (wr *Wrangler) VDiff2(ctx context.Context, keyspace, workflowName string, command vdiff2.VDiffAction, subCommand, uuid string,
	options *tabletmanagerdata.VDiffOptions) (*VDiffOutput, error) {

	log.Infof("VDiff2 called with %s, %s, %s, %s, %s, %+v", keyspace, workflowName, command, subCommand, uuid, options)

	req := &tabletmanagerdata.VDiffRequest{
		Keyspace:   keyspace,
		Workflow:   workflowName,
		Command:    string(command),
		SubCommand: subCommand,
		Options:    options,
		VdiffUuid:  uuid,
	}
	output := &VDiffOutput{
		Request:   req,
		Responses: make(map[string]*tabletmanagerdata.VDiffResponse),
		Err:       nil,
	}

	ts, err := wr.buildTrafficSwitcher(ctx, keyspace, workflowName)
	if err != nil {
		return nil, err
	}

	output.Err = ts.ForAllTargets(func(target *workflow.MigrationTarget) error {
		resp, err := wr.tmc.VDiff(ctx, target.GetPrimary().Tablet, req)
		output.mu.Lock()
		defer output.mu.Unlock()
		output.Responses[target.GetShard().ShardName()] = resp
		return err
	})
	if output.Err != nil {
		log.Errorf("Error in command %s: %w", command, output.Err)
		return nil, err
	}
	log.Infof("Output for %s is %+v", command, output)

	return output, nil
}
