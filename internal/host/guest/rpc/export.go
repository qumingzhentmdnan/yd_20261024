package rpc

import (
	"context"
	"safeagent/internal/protocol"
)

// ListChanges 获取停止后工作副本的有界文件清单。
func (c *Client) ListChanges(ctx context.Context, p protocol.ListChangesParams) (protocol.ChangePage, error) {
	var result protocol.ChangePage
	err := c.call(ctx, protocol.MethodListChanges, p, &result)
	return result, err
}

// ReadChangeChunk 获取停止后工作副本的有界文件内容。
func (c *Client) ReadChangeChunk(ctx context.Context, p protocol.ReadChangeChunkParams) (protocol.ChangeChunk, error) {
	var result protocol.ChangeChunk
	err := c.call(ctx, protocol.MethodReadChangeChunk, p, &result)
	return result, err
}
