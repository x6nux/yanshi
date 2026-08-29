# ADR-0019: 档位由 payload 决定，不由程序名决定

- 状态：accepted
- 日期：2026-08-29

## 背景（Context）

[ADR-0018](0018-an-unread-payload-is-a-refusal-not-a-pass.md) 新增了 `DestructionOpaque` 档：读不懂的 payload 从静默 Allow 变成 Prompt。档位选 Prompt 而不是结构性 HardDeny，理由写得很清楚 —— 「不可申诉的拒绝只有在理由能被陈述时才站得住，而『这里有段我读不懂的东西』是要求问一次的理由」，替代方案 C（判成结构性）因此被否决。

**那条理由是对着 `python3 -c "…"` 写的，却被应用到了一个它并不描述的形态上。** 复评实测：

```
bash -c "rm -rf /"      HardDeny(structural)   任何模式都拦
fish -c "rm -rf /"      Prompt                 yolo 直接跑
```

`mksh` / `yash` / `elvish` / `nu` 同样。两条命令是**同一条命令**，唯一的差别是一个 shell 的名字在 `posixShellPrograms` 里、另一个不在。于是「换一个 guard 没听说过的 shell」成了一条**通用的 yolo 绕过** —— 而「没人听说过的 shell」这个集合，正是 `opaque.go` 的包头说程序名表永远追不完的那种无界集合。

裁定（W-B-9）把这件事定性为：**W-B-7 写的「档位可覆盖、yolo 仍能放行」这句话写窄了。** 它对「读不懂 payload」是对的，对「读得懂 payload、而且读出来是灾难」是错的。

### 被否决的替代方案

**A. 把 `fish` / `nu` / `mksh` / `yash` / `elvish` 加进 `posixShellPrograms`。** 前六轮做过六次的动作。`oil`、`xonsh`、`hush`、明天发布的第七个都还在外面，默认仍然是弱档。

**B. 把整个 `DestructionOpaque` 升级成结构性 HardDeny。** 这就是 ADR-0018 的替代方案 C，它被否决的理由今天仍然成立：`python3 -c "print(1)"` 会在 default 模式下永久不可用、在 yolo 下不可申诉。

**C. 在 `resolvePermissionMode` 里给 Opaque 单开一条 yolo 拦截。** 把判据搬到消费侧，等于让 guard 包外的一个 switch 决定 guard 的档位语义；而 `Decision` 与 `ClassifyDestruction` 两个消费点都要各改一次（漏一个就是「Decision 是结构性拒绝、分类值仍是 Opaque」，yolo 照样自动放行）。

## 决策（Decision）

**payload 若能被读成 shell 命令、且那个读法判为 Catastrophic，则无论哪个程序接收它，判决都是结构性的那一档。读不出灾难性的读法时，档位仍是 `DestructionOpaque`（Prompt）。**

落点是 `internal/guard/opaque.go::gradeUnreadPayload`：`opaquePayload` 交回的那段操作数被送回 `classifyDestruction` 再读一次，`>= DestructionCatastrophic` 时原样返回（`DestructionUnreadable` 一并透传 —— 它同样是结构性档，而且它的 reason 说的是真正发生的事），否则返回 `DestructionOpaque`。

于是：

- `fish -c "rm -rf /"` 与 `bash -c "rm -rf /"` **同判**。
- `python3 -c "print(1)"`、`psql -c "select 1"`、`perl -e "unlink '/etc/passwd'"`（越界删除，不是灾难档）**仍然只是弹窗**。

**程序名是无界的，payload 的危险性是有界的** —— 这是本条与 ADR-0018 的分工：0018 说「读不懂的时候怎么办」，本条说「档位该从哪一侧读出来」。

## 后果（Consequences）

> 含**不可违反的约束**（加粗）。

- **修正 ADR-0018 的一条不可违反约束。** 那条写的是「`DestructionOpaque` 必须是 Prompt，不得升级为结构性 HardDeny」。**它继续成立，但作用域被收窄为「payload 读不出灾难性读法」的情况** —— 那正是它举证时用的 `python3 -c`。看守仍是 `internal/guard::TestOpaqueIsNotTheStructuralFloor`（它断言的就是 `python3 -c "print(1)"` 的 `Verdict == Prompt && Promptable`）。
- **不可违反的约束：判据必须落在 payload 上，不得退化成一张 shell 名字表。** 加一个 shell 名字进 `posixShellPrograms` 只是让某一条读得更准，不改变默认；把本条实现成「已知危险 shell 列表」会把无界的那一半原样搬回来。看守是 `internal/guard::TestTheTierFollowsThePayloadNotTheProgramName` 的第一组，其中一行用的是 `internal/guard::unmodelledInterpreter` —— 一个由 `internal/guard::TestNoTableModelsTheProbeProgram` 证明**不在本包任何表里**的程序名。
- **不可违反的约束：非灾难性 payload 必须留在 Prompt。** 第二组断言的就是这一半；没有它，第一组可以靠「所有 `-c` 一律拒绝」满足，而那是 ADR-0018 替代方案 C 的形态。
- **不可违反的约束：`ClassifyDestruction` 返回的值本身必须是灾难档，不能只让 `Decision` 变成结构性拒绝。** `internal/api/http::resolvePermissionMode` 的 fail-safe 读的是 `ClassifyDestruction` 而不是 `Decision`，两者分叉时 yolo 会绕过。看守是同一条测试里的 yolo 半段。
- **过严的代价是真的，而且这个代价此前一直在付。** 一段仅仅**包含**灾难命令的文本（`zzsend -c "rm -rf / is dangerous"`）会被不可申诉地拒绝。这不是新增的一类过严：`posixShellPrograms` 里的每一个 shell 从灾难档存在起就是这么判的，本条改的只是「它取决于那个 shell 的名字在不在表里」。
- 本条**不解决**「读懂另一种语言的 payload」。`python3 -c "import shutil;shutil.rmtree('/')"` 仍然是弹窗，因为把它读成 shell 命令的结果是 None —— 那是 ADR-0018 替代方案 D，另一个工作包。

## 关联

- 修正：[ADR-0018](0018-an-unread-payload-is-a-refusal-not-a-pass.md)（档位那一条约束的作用域）。
- 来源：W-B 第二批第二轮验证 `verify-b2.md` 的 V-9，与其上的裁定 W-B-9。
- 相关代码落点：`internal/guard/opaque.go`（`gradeUnreadPayload`）、`internal/guard/destructive.go`（`classifyLexed` 的 `!read` 分支）。
- 与 [ADR-0016](0016-two-shell-readers-one-word-layer.md) 的分工：0016 说「哪个 reader 读这条命令」，本条说「读出来之后档位从哪一侧取」。
