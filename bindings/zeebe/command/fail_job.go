/*
Copyright 2021 The Dapr Authors
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

package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/camunda/zeebe/clients/go/v8/pkg/commands"

	"github.com/dapr/components-contrib/bindings"
	"github.com/dapr/kit/metadata"
)

var ErrMissingRetries = errors.New("retries is a required attribute")

type failJobPayload struct {
	JobKey       *int64             `json:"jobKey"`
	Retries      *int32             `json:"retries"`
	ErrorMessage string             `json:"errorMessage"`
	RetryBackOff *metadata.Duration `json:"retryBackOff,omitempty"`
	Variables    interface{}        `json:"variables"`
}

func (z *ZeebeCommand) failJob(ctx context.Context, req *bindings.InvokeRequest) (*bindings.InvokeResponse, error) {
	var payload failJobPayload
	err := json.Unmarshal(req.Data, &payload)
	if err != nil {
		return nil, err
	}

	if payload.JobKey == nil {
		return nil, ErrMissingJobKey
	}

	if payload.Retries == nil {
		return nil, ErrMissingRetries
	}

	cmd := z.client.NewFailJobCommand().
		JobKey(*payload.JobKey).
		Retries(*payload.Retries)

	if payload.ErrorMessage != "" {
		cmd = cmd.ErrorMessage(payload.ErrorMessage)
	}

	if payload.RetryBackOff != nil && payload.RetryBackOff.Duration != time.Duration(0) {
		cmd = cmd.RetryBackoff(payload.RetryBackOff.Duration)
	}

	var cmdDispatch commands.DispatchFailJobCommand = cmd
	if payload.Variables != nil {
		cmdDispatch, err = cmd.VariablesFromObject(payload.Variables)
		if err != nil {
			return nil, err
		}
	}

	_, err = cmdDispatch.Send(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot fail job for key %d: %w", payload.JobKey, err)
	}

	return &bindings.InvokeResponse{}, nil
}
