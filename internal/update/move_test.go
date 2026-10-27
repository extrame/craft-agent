package update

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fastMoves 把改名重试的间隔压成毫秒级，免得每个用例真的等六秒。
func fastMoves(t *testing.T) {
	t.Helper()
	old := moveWaits
	moveWaits = []time.Duration{time.Millisecond}
	t.Cleanup(func() { moveWaits = old })
}

// 正常情况：.part 变成正式名字，内容不变。
func TestMoveIntoPlaceRenames(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "x.dmg.part")
	dest := filepath.Join(dir, "x.dmg")
	if err := os.WriteFile(part, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveIntoPlace(part, dest); err != nil {
		t.Fatalf("moveIntoPlace: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "payload" {
		t.Fatalf("dest 内容不对：%q %v", got, err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf(".part 应当已经不在：%v", err)
	}
}

// 同版本重装时 dest 已经存在，新的应当覆盖旧的。
func TestMoveIntoPlaceOverwritesStale(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "x.dmg.part")
	dest := filepath.Join(dir, "x.dmg")
	os.WriteFile(part, []byte("new"), 0o644)
	os.WriteFile(dest, []byte("old-and-longer"), 0o644)
	if err := moveIntoPlace(part, dest); err != nil {
		t.Fatalf("moveIntoPlace: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "new" {
		t.Fatalf("旧包没被覆盖：%q", got)
	}
}

// 改名一直失败时，唯一不能丢的是已经下完的 .part。
func TestMoveIntoPlaceKeepsPartialOnFailure(t *testing.T) {
	fastMoves(t)
	dir := t.TempDir()
	part := filepath.Join(dir, "x.dmg.part")
	dest := filepath.Join(dir, "x.dmg")
	if err := os.WriteFile(part, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 目标是个非空目录：既盖不掉，也打不开来写。
	if err := os.MkdirAll(filepath.Join(dest, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := moveIntoPlace(part, dest)
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "重试不会重新下载") {
		t.Errorf("错误里要说明下载已保留：%v", err)
	}
	if _, statErr := os.Stat(part); statErr != nil {
		t.Fatalf(".part 被删掉了，下载作废：%v", statErr)
	}
}

// 改名失败过一次之后，用户重试不该再花一次流量。
func TestDownloadSurvivesRenameFailure(t *testing.T) {
	fastMoves(t)

	data := payload(64 << 10)
	f := &flaky{data: data}
	srv := httptest.NewServer(f)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.zip")
	if err := os.MkdirAll(filepath.Join(dest, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := newTestClient().Download(context.Background(), assetFor(data, srv.URL), dest, nil); err == nil {
		t.Fatal("第一次应当失败：目标位置被目录占着")
	}
	part := dest + ".part"
	info, statErr := os.Stat(part)
	if statErr != nil {
		t.Fatalf(".part 被删了，等于白下：%v", statErr)
	}
	if info.Size() != int64(len(data)) {
		t.Fatalf(".part 不完整：%d / %d", info.Size(), len(data))
	}

	callsBefore := f.count()
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	if err := newTestClient().Download(context.Background(), assetFor(data, srv.URL), dest, nil); err != nil {
		t.Fatalf("障碍消失后重试应当成功：%v", err)
	}
	if f.count() != callsBefore {
		t.Fatalf("重试又发了请求：%d → %d", callsBefore, f.count())
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(data) {
		t.Fatalf("最终内容不对：%d 字节", len(got))
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	body := strings.Repeat("0123456789", 10_000)
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("复制内容不一致：%d 字节 vs %d", len(got), len(body))
	}
}
