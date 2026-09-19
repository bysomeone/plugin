package executor

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件钉住 #47 的另一半：**已删除的配置键不得再出现在运维提示里**。
//
// 背景：配置项 btcHeaderStartHeight 已删除（头链 bootstrap 的提交起点改为由 btcd chaincfg 的
// 内置锚点本地推出，见 lightclient neutrino 的 btcHeaderStartHeight()）。执行器侧的运维提示
// （bootstrap 锚点被拒时的 reject detail / hint 文案）随后也改掉了指向它的说法
// （提交 88d4ad545：改为报 expectedRelayStartHeight + "not a config item"，并把成因指向
// "两边 btcd 不同源"）。**但当时没有断言钉住这一点** —— 提示文案一旦回退（例如 hint 又写成
// "请调 btcHeaderStartHeight"），运维会去找一个**不存在**的键，而所有测试仍然全绿。
//
// 判据的取法（为什么只扫**字符串字面量**，不扫注释、也不扫标识符）：
//   - btcHeaderStartHeight 作为**标识符**仍然合法：它还是中继（neutrino）里那个推导函数的函数名，
//     执行器侧的注释也仍然需要解释"起点从哪来"（这些都是对的，不该被判成回归）；
//   - 运维/调用方真正能看到的只有**字符串字面量**（错误与提示文案、hint 文本）。
//     所以只扫字面量：既钉住"提示不再指向这个已删除的键"，又不会误伤正当的注释与函数名。
//
// 为什么放在 rgbx/executor 而不是 lightclient/executor：被钉住的是 lightclient 的文案，但本用例
// 是**跨模块契约**的回归护栏（谁改谁的红）。放在消费侧与依赖方向一致 —— rgbx 的共识判定正是这份
// 头查询的调用方，提示里指向不存在的键会直接把排障的人引向错误方向。
//
// 与既有覆盖的关系（本用例**不**重复它们）：
//   - "旧键不被**读**"（老 TOML 里留着该键无害、配置结构里已无该字段）由 neutrino 的
//     Test_legacyBtcHeaderStartHeightKeyIsIgnored 覆盖（那是配置解析侧，本用例管不到）；
//   - 本用例管的是**对外文案**这一面：提示不得再点名一个已经被删掉的键。
func Test_noStaleBtcHeaderStartHeightKnobInLightclientHints(t *testing.T) {
	root := filepath.Join("..", "..", "lightclient")
	require.DirExists(t, root, "用例前提：lightclient 源码在仓库里（否则本用例什么都没测到）")

	const staleKey = "btcHeaderStartHeight"

	var scanned int
	var offenders []string

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// 解析不出来的文件不能算"通过"：那样护栏会静默失效（改了文件却没人发现）。
			offenders = append(offenders, fmt.Sprintf("%s (parse error: %v)", path, perr))
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				s = lit.Value
			}
			if strings.Contains(s, staleKey) {
				offenders = append(offenders, fmt.Sprintf("%s:%d", path, fset.Position(lit.Pos()).Line))
			}
			return true
		})
		return nil
	})
	require.NoError(t, walkErr)
	require.Positive(t, scanned, "用例前提：至少扫到一个 lightclient 源文件（扫描面失效时必须变红）")
	require.Empty(t, offenders,
		"运维提示里又出现了已删除的配置键 %q：该键已不存在（起点由 btcd 内置锚点本地推出，"+
			"没有可改的配置项），提示必须指向真正的成因（两边 btcd 不同源 / 去掉额外锚点）。命中位置：%v",
		staleKey, offenders)
}
