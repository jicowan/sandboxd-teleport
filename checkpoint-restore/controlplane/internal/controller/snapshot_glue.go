/*
Copyright 2026.

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

package controller

import (
	"context"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jicowan/aio-sandbox/controlplane/internal/snapshot"
)

// BuildSnapshotStore builds a snapshot.Store with an S3 client from the AWS default
// chain (the operator's Pod Identity). Copy-on-promote needs s3:GetObject on
// sandboxes/* (read the source), s3:PutObject on bases/* (write the copy), and the
// reaper needs s3:ListBucket + s3:DeleteObject on bases/* — a superset of the GC's
// sandboxes/*-only policy, provisioned out-of-band (docs/PRD-snapshot-fork.md §10).
func BuildSnapshotStore(ctx context.Context, bucket string) (*snapshot.Store, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return snapshot.New(s3.NewFromConfig(cfg), bucket), nil
}
