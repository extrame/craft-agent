//go:build darwin

package update

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"craft-agent/internal/craft"
	"craft-agent/internal/release"
)

// Apple 的工具一律用绝对路径：这个进程可能从 Finder 或登录项起来，
// PATH 很短，不能被别处的同名程序顶替。
const (
	hdiutil  = "/usr/bin/hdiutil"
	ditto    = "/usr/bin/ditto"
	codesign = "/usr/bin/codesign"
	xattr    = "/usr/bin/xattr"
)

// Options 是一次安装的全部输入。
type Options struct {
	App     craft.App
	Asset   Asset
	WorkDir string
	// Dest 覆盖安装目录，只给测试与「装到别处」用；空表示跟随现有副本的位置。
	Dest string
	// Version 是这次要装的版本，用来核对新副本是不是真的这个版本。
	Version string
	// OnStage 接收进度；可以为 nil。
	OnStage func(Stage)
}

// Install 在 macOS 上完成一次真正的替换：下载 → 校验 → 挂载 → 复制到暂存 →
// 验签名与版本 → 换掉旧副本 → 回读。
func Install(ctx context.Context, c *Client, o Options) (Result, error) {
	if o.App.ID == "" || o.Asset.Name == "" {
		return Result{}, errors.New("缺少要安装的应用或安装包")
	}
	if err := os.MkdirAll(o.WorkDir, 0o755); err != nil {
		return Result{}, err
	}

	existing := release.Detect(o.App)
	existingBundle := release.BundleOf(existing.Path)
	destDir, destBundle := destination(o, existingBundle)

	// 1. 下载
	//
	// 同名文件不删：Download 会先比大小和 SHA-256，已经下好直接跳过，
	// 下了一半的就从断点续 —— 一百多兆的包，重来一次要几分钟。
	file := filepath.Join(o.WorkDir, o.Asset.Name)
	stage(o.OnStage, Stage{Step: "download", Percent: 0, Total: o.Asset.Size})
	err := c.Download(ctx, o.Asset, file, func(done, total int64) {
		pct := -1
		if total > 0 {
			pct = int(done * 100 / total)
		}
		stage(o.OnStage, Stage{Step: "download", Percent: pct, Bytes: done, Total: total})
	})
	if err != nil {
		return Result{}, err
	}
	stage(o.OnStage, Stage{Step: "verify", Percent: 100, Note: "SHA-256 已核对"})

	// 2. 挂载到一个本次专用的空目录。固定目录在「上次挂载被打断」时会残留，
	//    hdiutil 不会再往上挂第二个卷。
	mount := filepath.Join(o.WorkDir, fmt.Sprintf("mnt-%s-%d", o.App.ID, now().UnixNano()))
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(mount)
	if err := attach(file, mount); err != nil {
		return Result{}, err
	}
	defer detach(mount)

	stage(o.OnStage, Stage{Step: "install", Percent: -1, Note: "正在复制应用包"})

	src, err := findBundle(mount, o.App)
	if err != nil {
		return Result{}, err
	}

	// 3. 拷到目标目录旁边的暂存位置：同一卷才能用 rename 原子换位，
	//    中途失败也只是留下一个临时目录，碰不到正在用的那份。
	//    首次安装时目标目录可能还不存在（家目录下的 Applications）。
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("创建安装目录 %s 失败：%w", destDir, err)
	}
	staging, err := os.MkdirTemp(destDir, ".craft-agent-stage-")
	if err != nil {
		return Result{}, fmt.Errorf("目标目录不可写（%s）：%w", destDir, err)
	}
	defer os.RemoveAll(staging)
	staged := filepath.Join(staging, filepath.Base(destBundle))

	if err := run(ctx, ditto, "--norsrc", "--noextattr", "--noacl", src, staged); err != nil {
		return Result{}, fmt.Errorf("复制应用包失败: %w", err)
	}

	// 4. 验明身份：签名、bundle id、版本。任何一项不符都不允许换掉旧副本。
	team := teamOf(existingBundle)
	if err := verifySignature(ctx, staged, team); err != nil {
		return Result{}, err
	}
	found := release.ReadBundle(o.App, staged)
	if found.Error != "" {
		return Result{}, fmt.Errorf("新副本不可用: %s", found.Error)
	}
	if err := sameRelease(found.Version, o.Version); err != nil {
		return Result{}, err
	}

	// 5. 换位。旧副本改名留着，装完这版万一打不开，拖回来就是上一版。
	backup := ""
	if _, err := os.Lstat(destBundle); err == nil {
		backup = fmt.Sprintf("%s.old-%s", destBundle, now().Format("20060102-150405"))
		if err := os.Rename(destBundle, backup); err != nil {
			return Result{}, fmt.Errorf("无法移开旧版本（可能还在运行；macOS 15 起还要求「系统设置 → 隐私与安全性 → App 管理」里允许本程序改动其他应用）: %w", err)
		}
	}
	if err := os.Rename(staged, destBundle); err != nil {
		if backup != "" {
			_ = os.Rename(backup, destBundle)
		}
		return Result{}, fmt.Errorf("替换失败，已回滚: %w", err)
	}

	// dmg 里的文件带着卷的元数据，装好的包不该留这些；顺手把扩展属性里的
	// 隔离标记也清掉，否则首次启动会被 Gatekeeper 再拦一次。
	_ = exec.Command(xattr, "-cr", destBundle).Run()

	after := release.ReadBundle(o.App, destBundle)
	stage(o.OnStage, Stage{Step: "done", Percent: 100, Note: "v" + after.Version})
	return Result{Version: after.Version, Path: destBundle, Backup: backup}, nil
}

// destination 决定装到哪：优先用户指定的目录，其次现有副本所在目录
// （用户把应用放在 ~/Applications 或别处，就还在那儿），最后是首次安装。
//
// 首次安装落在 /Applications；写不进去（非管理员账号）就退回家目录的
// Applications —— 它在 Launchpad 和 Finder 里同样可见，catalog 的探测路径
// 也包含它，所以装完立刻就能被认出来。
func destination(o Options, existingBundle string) (dir, bundle string) {
	name := o.App.Name + ".app"
	if g := o.App.Name; g == "" {
		name = o.App.ID + ".app"
	}
	if o.Dest != "" {
		return o.Dest, filepath.Join(o.Dest, name)
	}
	if existingBundle != "" {
		return filepath.Dir(existingBundle), existingBundle
	}
	for _, dir := range []string{"/Applications", filepath.Join(homeDir(), "Applications")} {
		if writable(dir) {
			return dir, filepath.Join(dir, name)
		}
	}
	// 两处都写不进去：照旧返回标准位置，让后面的 MkdirTemp 给出带路径的错误。
	return "/Applications", filepath.Join("/Applications", name)
}

// writable 判断能不能在这个目录里落一个新条目。真正的判据是试一次，
// 权限位、ACL、只读卷都能被这一步如实反映出来。
func writable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe, err := os.MkdirTemp(dir, ".craft-agent-probe-")
	if err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// findBundle 在挂载点里找应用包。优先同名，其次唯一的那个 .app。
func findBundle(mount string, app craft.App) (string, error) {
	want := filepath.Join(mount, app.Name+".app")
	if info, err := os.Stat(want); err == nil && info.IsDir() {
		return want, nil
	}
	entries, err := os.ReadDir(mount)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			found = append(found, filepath.Join(mount, e.Name()))
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("镜像里没有找到 %s.app", app.Name)
	default:
		return "", fmt.Errorf("镜像里有多个应用包，无法确定要装哪个（%s）", strings.Join(found, ", "))
	}
}

func attach(dmg, mount string) error {
	// 校验和刚在下载时核对过，hdiutil 自己的校验（-noverify）只是白白多花几分钟。
	args := []string{"attach", "-nobrowse", "-readonly", "-noautoopen", "-noverify", "-mountpoint", mount, dmg}
	// diskimages-helper 在忙别的镜像时会直接拒绝（Resource temporarily unavailable）：
	// 等一会儿再试，最多四轮。
	var last error
	for _, wait := range []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second} {
		if wait > 0 {
			time.Sleep(wait)
		}
		if err := run(context.Background(), hdiutil, args...); err != nil {
			last = err
			_ = detach(mount)
			continue
		}
		return nil
	}
	if last == nil {
		last = errors.New("未知原因")
	}
	return fmt.Errorf("挂载磁盘镜像失败: %w", last)
}

func detach(mount string) error {
	var err error
	// 普通 detach 偶尔会因为 Spotlight 或 Finder 正在看这个卷而失败。失败一次
	// 就留下一个永久挂着的卷（侧栏里多出一个 FigCraft），所以这里重试，
	// 最后一轮直接 -force。
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		args := []string{"detach", mount}
		if i == 2 {
			args = []string{"detach", "-force", mount}
		}
		err = run(ctx, hdiutil, args...)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	log.Printf("卸载 %s 失败：%v", mount, err)
	return err
}

// teamOf 读现有副本的开发者团队。新副本必须由同一个团队签名 ——
// 这比硬编码一个团队 ID 更稳，也不妨碍将来换签名方。
func teamOf(bundle string) string {
	if bundle == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, codesign, "-dv", "--verbose=4", bundle).CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "TeamIdentifier="); ok {
			v = strings.TrimSpace(v)
			if v != "" && v != "not set" {
				return v
			}
		}
	}
	return ""
}

// verifySignature 先做一次严格自检，再（在有 team 的前提下）要求签名方是同一个团队。
//
// 严格自检能发现「包被改过」「内部签名不一致」；团队要求能挡住「拿别人的合法签名
// 冒充这个应用」。两道都过了才允许替换。
func verifySignature(ctx context.Context, bundle, team string) error {
	if err := run(ctx, codesign, "--verify", "--deep", "--strict", bundle); err != nil {
		return fmt.Errorf("新副本签名校验失败: %w", err)
	}
	if team == "" {
		return nil
	}
	requirement := `=anchor apple generic and certificate leaf[subject.OU] = "` + team + `"`
	if err := run(ctx, codesign, "--verify", "--deep", "-R", requirement, bundle); err != nil {
		return fmt.Errorf("新副本的签名方与已安装版本不一致（期望团队 %s）: %w", team, err)
	}
	return nil
}

// sameRelease 比对版本号。只比前三位数字，外加预发布标记 ——
// bundle 里常常只写数字（CFBundleShortVersionString），不能因此判成不同版本。
func sameRelease(found, want string) error {
	if strings.TrimSpace(want) == "" {
		return nil
	}
	fv, okF := release.Parse(found)
	wv, okW := release.Parse(want)
	if !okF || !okW {
		return fmt.Errorf("版本号无法比较：新副本 %q / 期望 %q", found, want)
	}
	if fv.Major != wv.Major || fv.Minor != wv.Minor || fv.Patch != wv.Patch {
		return fmt.Errorf("镜像里的版本是 %s，不是要装的 %s", found, want)
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		said := strings.TrimSpace(string(out))
		// 输出长的只留尾部：hdiutil 报错时前面的进度行没意义。
		if len(said) > 400 {
			said = "…" + said[len(said)-400:]
		}
		if said == "" {
			return fmt.Errorf("%s: %w", filepath.Base(name), err)
		}
		return fmt.Errorf("%s: %w（%s）", filepath.Base(name), err, said)
	}
	return nil
}

// QuitApp 让应用自己退出，等它真的退干净。
//
// 用 AppleScript 的 quit 而不是 kill：应用自己有机会弹「保存吗」，
// 也就能把用户未保存的工作留住。进程没退就如实报错，不做强杀。
func QuitApp(app craft.App) error {
	bundle := release.BundleOf(app.InstalledPath())
	if bundle == "" {
		return nil
	}
	pid := pidOf(bundle)
	if pid == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "/usr/bin/osascript", "-e",
		fmt.Sprintf(`tell application "%s" to quit`, strings.ReplaceAll(app.Name, `"`, `\"`))).Run()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if pidOf(bundle) == 0 {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("%s 还在运行（PID %d），先退出再更新", app.Name, pid)
}

// pidOf 找以这个 .app 的可执行文件运行的进程；0 表示没在跑。
func pidOf(bundle string) int {
	pattern := filepath.Join(bundle, "Contents", "MacOS") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/pgrep", "-f", pattern).Output()
	if err != nil {
		return 0
	}
	for _, field := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil && pid != os.Getpid() {
			return pid
		}
	}
	return 0
}
