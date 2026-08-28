# ADR-0017: 参数展开是第二种读法，不是把命令改写掉

- 状态：accepted
- 日期：2026-08-29

## 背景（Context）

`Guard.Check` 的每一道维度读的都是 `Action.Shell` 的**字面文本**，而 POSIX shell 执行的是**展开之后**的文本。这两者的差被实测证明是可利用的：在一个「唯一可能的拒绝是结构性下限」的 profile 下，下面每一条都判 Allow，而真 `/bin/sh` 执行了 `rm -rf /`——

```
rm${IFS}-rf${IFS}/     ${x:-rm} -rf /     rm -rf ${x:-/}     X=rm; $X -rf /
${IFS}rm -rf /         ${x-rm} -rf /      rm -rf "${x:-/}"   X=/; rm -rf $X
set -- rm -rf /; "$@"
```

另有四条把公钥写进了 `~/.ssh/authorized_keys` 而没有任何提示，因为凭据 denylist 按 `~/.ssh` 这个**目录段前缀**匹配，段里插一个展开就不命中：

```
echo k > ~/.s${x}sh/authorized_keys      echo k > ~/${x:-.ssh}/authorized_keys
echo k > ~/.ssh${x}/authorized_keys      X=.ssh; echo k > ~/$X/authorized_keys
```

`rm -rf "${x:-/}"` 值得单独看：它不含 `$(`，`hasControlOperator` 不触发，既没被拆段也没被 ANSI-C 重解，是一条**完全直路**。

**当时的状态最危险的地方不是「没做展开」，而是「做了两个特例」**：`pathnorm.go` 已经解析 `~` 与 `$HOME`，删除门的 doc 也这么写着，所以整份文件读起来像是展开已经被处理过了。

### 被否决的替代方案

**A. 把每个未解析的展开当成空串**（这是 shell 对未设置变量的真实语义）。实测这是**结构性回归**：`rm -rf $BUILD_DIR` 变成裸 `rm -rf`，而「recursive 且无目标」正是 catastrophic 档，于是一条每个模型都写过的清理命令变成**任何模式都不可申诉**的拒绝。

**B. 遇到未解析的展开就 fail-closed 拒绝**（像 `$(` 那样）。同一个代价，换了个说法：`$PYTHON -m pip`、`rm -rf $TMPDIR/x` 全部变成结构性拒绝。`readers_test.go` 的包头已经写过这个方向「比多问一次更糟」。

**C. 真的做展开**。那是写一个 shell。

## 决策（Decision）

**展开是加进来的第二种读法，与既有的「转义读法」（`unescapeWordLetters`）平级，用 `moreSevere` 折叠，永远不替换第一种。**

`expandKnownParameters` 只解析**值就在命令字符串里**的展开：`${IFS}`（POSIX 默认值是空格）、`${v:-w}` 一族写在文本里的默认值、同一条命令里更早出现的赋值（含 `export`）、更早出现的 `set --` 给出的位置参数。值来自字符串之外的展开**原样保留**，与改动前的行为逐字节一致。

**凭据维度取相反的决定，并且是刻意的。** `IsSensitivePath` 对含展开的路径额外试一次「把展开抹掉」的读法（`elideExpansions`），因为 denylist 匹配的是字面目录段，而 `~/.s${x}sh` 就是 `~/.ssh` 里插了一个空展开。

## 后果（Consequences）

- 两条读法都只能**加严**，所以任何一条新的解析规则都不可能放宽已有判决。
- 展开读法只跑一次：`Check` 调不递归的 `check` 来评第二种读法，展开后的字符串不会再被展开。
- 凭据方向多出的读法最坏结果是「对一条长得像凭据路径的路径多问一次」，而 `sensitive.go` 的档位本来就是 Prompt + 字面授权逃生门。
- **不可违反的约束：删除门这一侧，值不在命令字符串里的展开不得被抹成空串，也不得因为存在而触发拒绝。** 两者都会把 `rm -rf $BUILD_DIR` 变成不可申诉的结构性拒绝。语料里 `internal/guard::TestExpandKnownParametersResolvesOnlyWhatTheStringDefines` 与 `bypasscorpus_test.go` 的 `rm -rf $BUILD_DIR` / `$UNSET_PROGRAM -rf /` 两行是它的看守，`BUILD_DIR=build; rm -rf $BUILD_DIR` 是配套的正向对照（没有它，前两行可以靠「什么都不解析」满足）。
- **不可违反的约束：凭据维度与删除门在「未解析的展开」上取相反决定，这个不对称必须留着，并且要写在代码里。** 抹空对**路径**是正确读法、对**删除**是灾难，理由是两侧的失败方向不同：前者多一次提示，后者多一次不可申诉的拒绝。把两边「统一」成任何一侧都会重新打开其中一组绕过。
- **不可违反的约束：`${v:+w}` / `${v+w}` 不解析。** 它们的 `w` 只在变量**已设置**时生效，而这个读法对变量一无所知；解析它等于凭空发明一个值。

## 关联

- 来源：W-B 第二批复评 B-1；`docs/superpowers/review-checklist.md` 的 A 段变异手法。
- 相关代码落点：`internal/guard/expansion.go`、`internal/guard/guard.go`（`Guard.Check` 的两读折叠）、`internal/guard/sensitive.go`（`IsSensitivePath`）。
- 与 [ADR-0016](0016-two-shell-readers-one-word-layer.md) 的分工：0016 说的是**结构层**按语言分两个 reader，本条说的是**词的值**这一层加一种读法；两者正交，词层仍然共享。
