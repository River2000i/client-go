// Copyright 2021 TiKV Authors
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

// NOTE: The code in this file is based on code from the
// TiDB project, licensed under the Apache License v 2.0
//
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/test_util.go
//

// Copyright 2017 PingCAP, Inc.
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

package tikv

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/pingcap/kvproto/pkg/keyspacepb"
	"github.com/tikv/client-go/v2/internal/apicodec"
	"github.com/tikv/client-go/v2/internal/locate"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/util/async"
	pd "github.com/tikv/pd/client"
)

// CodecClient warps Client to provide codec encode and decode.
type CodecClient struct {
	Client
	codec apicodec.Codec
}

// SendRequest uses codec to encode request before send, and decode response before return.
func (c *CodecClient) SendRequest(ctx context.Context, addr string, req *tikvrpc.Request, timeout time.Duration) (*tikvrpc.Response, error) {
	req, err := c.codec.EncodeRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.SendRequest(ctx, addr, req, timeout)
	if err != nil {
		return nil, err
	}
	return c.codec.DecodeResponse(req, resp)
}

func (c *CodecClient) SendRequestAsync(ctx context.Context, addr string, req *tikvrpc.Request, cb async.Callback[*tikvrpc.Response]) {
	req, err := c.codec.EncodeRequest(req)
	if err != nil {
		cb.Invoke(nil, err)
		return
	}
	cb.Inject(func(resp *tikvrpc.Response, err error) (*tikvrpc.Response, error) {
		if err != nil {
			return nil, err
		}
		return c.codec.DecodeResponse(req, resp)
	})
	c.Client.SendRequestAsync(ctx, addr, req, cb)
}

// NewTestTiKVStore creates a test store with Option
func NewTestTiKVStore(client Client, pdClient pd.Client, clientHijack func(Client) Client, pdClientHijack func(pd.Client) pd.Client, txnLocalLatches uint, opt ...Option) (*KVStore, error) {
	codec := apicodec.NewCodecV1(apicodec.ModeTxn)
	client = &CodecClient{
		Client: client,
		codec:  codec,
	}
	pdCli := pd.Client(locate.NewCodecPDClient(ModeTxn, pdClient))

	if clientHijack != nil {
		client = clientHijack(client)
	}

	if pdClientHijack != nil {
		pdCli = pdClientHijack(pdCli)
	}

	// Make sure the uuid is unique.
	uid := uuid.New().String()
	spkv := NewMockSafePointKV()
	tikvStore, err := NewKVStore(uid, pdCli, spkv, client, opt...)

	if txnLocalLatches > 0 {
		tikvStore.EnableTxnLocalLatches(txnLocalLatches)
	}

	tikvStore.mock = true
	return tikvStore, err
}

// NewTestTiKVStore creates a test store with Option
func NewTestKeyspaceTiKVStore(client Client, pdClient pd.Client, clientHijack func(Client) Client, pdClientHijack func(pd.Client) pd.Client, txnLocalLatches uint, keyspaceMeta keyspacepb.KeyspaceMeta, opt ...Option) (*KVStore, error) {
	codec, err := apicodec.NewCodecV2(apicodec.ModeTxn, &keyspaceMeta)
	if err != nil {
		return nil, err
	}
	client = &CodecClient{
		Client: client,
		codec:  codec,
	}

	codecPDCli, err := locate.NewCodecPDClientWithKeyspace(apicodec.ModeTxn, pdClient, keyspaceMeta.Name)
	if err != nil {
		return nil, err
	}
	pdCli := pd.Client(codecPDCli)

	if clientHijack != nil {
		client = clientHijack(client)
	}

	if pdClientHijack != nil {
		pdCli = pdClientHijack(pdCli)
	}

	// Make sure the uuid is unique.
	uid := uuid.New().String()

	keyspaceIdStr := strconv.FormatUint(uint64(keyspaceMeta.Id), 10)
	spkv := NewMockSafePointKV(WithPrefix(keyspaceIdStr))
	tikvStore, err := NewKVStore(uid, pdCli, spkv, client, opt...)

	if txnLocalLatches > 0 {
		tikvStore.EnableTxnLocalLatches(txnLocalLatches)
	}

	tikvStore.mock = true
	return tikvStore, err
}
