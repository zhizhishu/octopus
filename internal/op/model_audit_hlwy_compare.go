package op

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// HlwyRecompare 用**已保存的原始数字**和指定基准离线重算一次对照。
//
// 它是一条纯函数式的读路径：不打上游、不建任务、不进队列、不写任何一行。
// 理由很实际——「换一份基准再看看」是分析动作，重复付款买同一批样本没有意义；
// 而一旦允许它写库，历史报告就会在没人察觉的时候被覆盖，
// 再想回答「当时看到的是什么」就没证据了。
//
// 可信边界：
//   - 可用性只认任务种类（job.kind=hlwy_compare）与报告快照，不看请求参数；
//   - 数字只取服务器报告里冻结的 raw_numbers，客户端传什么都不影响；
//   - 主对照优先用报告/选项里冻结的快照（同 id 不回查目录），只有换成新 id
//     才去目录里查——这是本功能的意义所在；
//   - 报告里存着的旧分数一律**不复用**：全部由 numberdist.Compare 重算，
//     否则读到的就是「上次的结论」，而本次换了基准它已不成立；
//   - 与本轮不可对照（题目/解析/协议不同）的基准不出分数，只给原因码。
func HlwyRecompare(ctx context.Context, resultID int64, baselineID string) (model.HlwyAuditReport, error) {
	if resultID <= 0 {
		return model.HlwyAuditReport{}, errors.New("缺少检测结果 id")
	}
	res, job, err := hlwyResultAndJob(ctx, resultID)
	if err != nil {
		return model.HlwyAuditReport{}, err
	}
	switch res.Status {
	case model.AuditResultPending, model.AuditResultRunning:
		return model.HlwyAuditReport{}, fmt.Errorf(
			"结果 #%d 还在 %s，没有可重算的采样快照", resultID, res.Status)
	}

	base, err := model.ParseHlwyReport(res.Report)
	if err != nil {
		return model.HlwyAuditReport{}, err
	}
	bc := base.BaselineComparison

	opts, err := model.ParseHlwyOptions(job.OptionsJSON)
	if err != nil {
		return model.HlwyAuditReport{}, fmt.Errorf("任务 #%d 的选项不可用: %w", job.ID, err)
	}

	chosenID := strings.TrimSpace(baselineID)
	if chosenID == "" {
		chosenID = bc.ChosenBaselineID
	}
	if chosenID == "" {
		chosenID = opts.BaselineID
	}

	// 同 id 一律沿用报告/选项里冻结的那份快照：基准表里的行今天可能已被另一份
	// 内容替换（或干脆读不出来），而「当时是拿哪一份比的」必须可复现。只有换成
	// 本轮从未涉及的新 id，才去目录里查。
	chosen, frozen := hlwyFrozenChosen(bc.BaselineSnapshots, opts.Candidates, chosenID)
	sourceResultID := int64(0)
	if frozen {
		// 快照里没有来源结果 id（那是库行的元数据）。这里只补查这一个字段，
		// 仅用于「自比」标记；查不到就当作不知道，不影响重算。
		sourceResultID = hlwyLocalSourceResultID(ctx, chosenID)
	} else {
		resolved, rerr := HlwyBaselineResolve(ctx, chosenID)
		if rerr != nil {
			return model.HlwyAuditReport{}, rerr
		}
		chosen = model.HlwySnapshotOf(resolved.Baseline)
		sourceResultID = resolved.SourceResultID
	}

	candidates, err := hlwyRecompareCandidates(opts.Candidates, chosen)
	if err != nil {
		return model.HlwyAuditReport{}, err
	}

	// 可对照程度必须先算：协议/解析/题目不同的基准压根不该出一条分数。
	comparability, codes := model.HlwyComparability(bc.Conditions, chosen.Conditions)

	// 报告里已经记下「本轮证据不可比」（题目/条件变过、算分坏过）时，
	// 离线重算也不能把它翻成一张带分数的表：换个基准不会让那批数字
	// 重新变成同一个实验的产物。
	if hlwyInvalidatesEvidence(bc.StopReason) {
		comparability = model.HlwyComparabilityIncompatible
		codes = append(codes, strings.TrimSpace(bc.StopReason))
	}

	cmp, err := numberdist.Compare(bc.RawNumbers, candidates, chosen.ID)
	if err != nil {
		return model.HlwyAuditReport{}, fmt.Errorf("重算失败: %w", err)
	}

	// 可评分 = 样本够 **且** 可对照。不可对照时任凭样本多少都不给分，
	// 把 Compare 算出的排名整份丢掉，只留一个能读的原因。
	scorable := model.HlwyScorable(cmp.ValidCount, comparability)
	matches := cmp.Matches
	reason := cmp.Reason
	// 不可比的理由优先于“样本不够”。
	if comparability == model.HlwyComparabilityIncompatible &&
		(reason == "" || reason == "insufficient_samples") {
		reason = model.HlwyReasonIncompatibleProtocol
	}
	if !scorable {
		matches = nil
	}
	selfCompare := sourceResultID != 0 && sourceResultID == resultID

	out := base
	out.Normalize()
	out.Kind = model.AuditKindHlwy
	if out.RequestedModel == "" {
		out.RequestedModel = hlwyResultModelName(res)
	}
	if out.UpstreamModel == "" {
		out.UpstreamModel = res.UpstreamModel
	}
	out.Score = 0
	out.Verdict = model.HlwyVerdictUnknown

	obc := out.BaselineComparison
	obc.SchemaVersion = numberdist.SchemaVersion
	obc.AlgorithmVersion = numberdist.AlgorithmVer
	obc.Comparability = comparability
	obc.ReasonCodes = codes
	obc.BaselineSnapshots = hlwySnapshotsOf(candidates)
	obc.Scorable = scorable
	obc.Reason = reason
	obc.ValidCount = cmp.ValidCount
	obc.Distribution = cmp.Distribution
	obc.Stats = cmp.Stats
	obc.ChosenBaselineID = chosen.ID
	obc.Matches = matches
	obc.Sufficiency = model.HlwySufficiency(cmp.ValidCount)
	obc.SelfCompare = selfCompare
	out.BaselineComparison = obc
	out.Plain = model.HlwyPlainOf(obc, selfCompare)

	// 交出去之前自校验一遍：任何字段被这次重算写坏，都在这里断掉，
	// 而不是让界面拿到一份自相矛盾的报告。
	if err := out.Validate(); err != nil {
		return model.HlwyAuditReport{}, fmt.Errorf("重算结果不自洽: %w", err)
	}
	return out.Filled(), nil
}

// hlwyRecompareCandidates 组装本次重算的候选集合。
//
// 候选 = 冻结的内置基准（它们是不可变的发布件，跨轮保留才有意义）+ 本次主对照。
// 之前选中的本地基准不保留：本地件是「某人某次采样」的快照，把上一份和这一份
// 同时排在一起，看着像在比较两个目标，其实是在比较两次采样。
//
// 超出上限时报错而不截断：悄悄砍掉一份候选，界面就会少一行排名，
// 而用户以为那是全部。
func hlwyRecompareCandidates(frozen []numberdist.Baseline, chosen numberdist.Baseline) ([]numberdist.Baseline, error) {
	if err := model.ValidateHlwyBaseline(chosen); err != nil {
		return nil, fmt.Errorf("指定的基准不可用: %w", err)
	}
	out := make([]numberdist.Baseline, 0, len(frozen)+1)
	for _, b := range frozen {
		if b.ID == chosen.ID || b.Source.Kind != numberdist.SourceBuiltin {
			continue
		}
		if err := model.ValidateHlwyBaseline(b); err != nil {
			return nil, fmt.Errorf("选项里的基准 %s 已损坏: %w", b.ID, err)
		}
		out = append(out, model.HlwySnapshotOf(b))
	}
	out = append(out, model.HlwySnapshotOf(chosen))
	if len(out) > model.HlwyMaxCandidates {
		return nil, fmt.Errorf("候选基准 %d 份超过上限 %d（无法加入 %s）",
			len(out), model.HlwyMaxCandidates, chosen.ID)
	}
	return out, nil
}

// hlwyFrozenChosen 在报告快照与选项候选里找同 id 的冻结基准（报告优先）。
func hlwyFrozenChosen(snapshots, candidates []numberdist.Baseline, id string) (numberdist.Baseline, bool) {
	for _, list := range [][]numberdist.Baseline{snapshots, candidates} {
		for _, b := range list {
			if b.ID == id {
				return model.HlwySnapshotOf(b), true
			}
		}
	}
	return numberdist.Baseline{}, false
}

// hlwySnapshotsOf 生成报告里的基准快照列表（逐份深拷贝）。
func hlwySnapshotsOf(baselines []numberdist.Baseline) []numberdist.Baseline {
	out := make([]numberdist.Baseline, 0, len(baselines))
	for _, b := range baselines {
		out = append(out, model.HlwySnapshotOf(b))
	}
	return out
}
