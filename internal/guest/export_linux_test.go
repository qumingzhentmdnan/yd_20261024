package guest

import (
	"context"
	"os"
	"path/filepath"
	"safeagent/internal/protocol"
	"testing"
)

// TestExportWorkspaceBoundaries 验证停止组门禁、链接拒绝及工作区读取。
func TestExportWorkspaceBoundaries(t *testing.T) {
	for _, scenario := range []string{"normal", "running", "symlink", "hardlink", "outside-parent"} {
		t.Run(scenario, func(t *testing.T) {
			b, c, _, _ := importFixture(t)
			project := filepath.Join(b.config.WorkspaceRoot.Name(), "project")
			if err := os.Mkdir(project, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, "file"), []byte("actual"), 0644); err != nil {
				t.Fatal(err)
			}
			b.importState = importCommitted
			switch scenario {
			case "running":
				b.sessions["run"] = &session{info: protocol.SessionInfo{GroupState: protocol.GroupStateRunning}}
			case "symlink":
				if err := os.Symlink("file", filepath.Join(project, "link")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(project, "file"), filepath.Join(project, "link")); err != nil {
					t.Fatal(err)
				}
			case "outside-parent":
				if err := os.Symlink(t.TempDir(), filepath.Join(project, "escape")); err != nil {
					t.Fatal(err)
				}
			}
			result, err := c.exportWorkspace(context.Background(), &protocol.ListChangesParams{})
			if scenario != "normal" {
				if err == nil {
					t.Fatal("接受不安全工作区")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page := result.(protocol.ChangePage)
			if len(page.Entries) != 1 || page.Entries[0].Path != "file" {
				t.Fatal(page)
			}
			result, err = c.exportWorkspace(context.Background(), &protocol.ReadChangeChunkParams{Path: "file"})
			if err != nil {
				t.Fatal(err)
			}
			if string(result.(protocol.ChangeChunk).Data) != "actual" {
				t.Fatal(result)
			}
			if _, err = c.exportWorkspace(context.Background(), &protocol.ReadChangeChunkParams{Path: "../outside"}); err == nil {
				t.Fatal("允许逃逸")
			}
		})
	}
}

// TestExportPagination 验证超过一页的清单游标及停止后冻结门禁。
func TestExportPagination(t *testing.T) {
	b, c, _, _ := importFixture(t)
	project := filepath.Join(b.config.WorkspaceRoot.Name(), "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		if err := os.WriteFile(filepath.Join(project, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	b.importState = importCommitted
	value, err := c.exportWorkspace(context.Background(), &protocol.ListChangesParams{})
	if err != nil {
		t.Fatal(err)
	}
	page := value.(protocol.ChangePage)
	if len(page.Entries) != 8 || page.Next != 8 {
		t.Fatal(page)
	}
	if _, err = protocol.Encode(page); err != nil {
		t.Fatal(err)
	}
	value, err = c.exportWorkspace(context.Background(), &protocol.ListChangesParams{Cursor: page.Next})
	if err != nil {
		t.Fatal(err)
	}
	page = value.(protocol.ChangePage)
	if len(page.Entries) != 1 || page.Next != 0 {
		t.Fatal(page)
	}
	b.mu.Lock()
	err = b.checkNewLocked(protocol.PrepareAgentParams{})
	b.mu.Unlock()
	if err == nil {
		t.Fatal("冻结后仍允许重新准备")
	}
}
