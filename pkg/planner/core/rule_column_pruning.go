// Copyright 2016 PingCAP, Inc.
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

package core

import (
	"context"
	"slices"

	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/planner/core/base"
	"github.com/pingcap/tidb/pkg/planner/util"
	"github.com/pingcap/tidb/pkg/planner/util/coreusage"
	"github.com/pingcap/tidb/pkg/planner/util/fixcontrol"
	"github.com/pingcap/tidb/pkg/planner/util/optimizetrace"
	"github.com/pingcap/tidb/pkg/planner/util/optimizetrace/logicaltrace"
)

type columnPruner struct {
}

func (*columnPruner) optimize(_ context.Context, lp base.LogicalPlan, opt *optimizetrace.LogicalOptimizeOp) (base.LogicalPlan, bool, error) {
	planChanged := false
	lp, err := lp.PruneColumns(slices.Clone(lp.Schema().Columns), opt)
	if err != nil {
		return nil, planChanged, err
	}
	return lp, planChanged, nil
}

// PruneColumns implement the Expand OP's column pruning logic.
// logicExpand is built in the logical plan building phase, where all the column prune is not done yet. So the
// expand projection expressions is meaningless if it built at that time. (we only maintain its schema, while
// the level projection expressions construction is left to the last logical optimize rule)
//
// so when do the rule_column_pruning here, we just prune the schema is enough.
func (p *LogicalExpand) PruneColumns(parentUsedCols []*expression.Column, opt *optimizetrace.LogicalOptimizeOp) (base.LogicalPlan, error) {
	// Expand need those extra redundant distinct group by columns projected from underlying projection.
	// distinct GroupByCol must be used by aggregate above, to make sure this, append DistinctGroupByCol again.
	parentUsedCols = append(parentUsedCols, p.DistinctGroupByCol...)
	used := expression.GetUsedList(p.SCtx().GetExprCtx().GetEvalCtx(), parentUsedCols, p.Schema())
	prunedColumns := make([]*expression.Column, 0)
	for i := len(used) - 1; i >= 0; i-- {
		if !used[i] {
			prunedColumns = append(prunedColumns, p.Schema().Columns[i])
			p.Schema().Columns = append(p.Schema().Columns[:i], p.Schema().Columns[i+1:]...)
			p.SetOutputNames(append(p.OutputNames()[:i], p.OutputNames()[i+1:]...))
		}
	}
	logicaltrace.AppendColumnPruneTraceStep(p, prunedColumns, opt)
	// Underlying still need to keep the distinct group by columns and parent used columns.
	var err error
	p.Children()[0], err = p.Children()[0].PruneColumns(parentUsedCols, opt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func pruneByItems(p base.LogicalPlan, old []*util.ByItems, opt *optimizetrace.LogicalOptimizeOp) (byItems []*util.ByItems,
	parentUsedCols []*expression.Column) {
	prunedByItems := make([]*util.ByItems, 0)
	byItems = make([]*util.ByItems, 0, len(old))
	seen := make(map[string]struct{}, len(old))
	for _, byItem := range old {
		pruned := true
		hash := string(byItem.Expr.HashCode())
		_, hashMatch := seen[hash]
		seen[hash] = struct{}{}
		cols := expression.ExtractColumns(byItem.Expr)
		if !hashMatch {
			if len(cols) == 0 {
				if !expression.IsRuntimeConstExpr(byItem.Expr) {
					pruned = false
					byItems = append(byItems, byItem)
				}
			} else if byItem.Expr.GetType(p.SCtx().GetExprCtx().GetEvalCtx()).GetType() != mysql.TypeNull {
				pruned = false
				parentUsedCols = append(parentUsedCols, cols...)
				byItems = append(byItems, byItem)
			}
		}
		if pruned {
			prunedByItems = append(prunedByItems, byItem)
		}
	}
	logicaltrace.AppendByItemsPruneTraceStep(p, prunedByItems, opt)
	return
}

// PruneColumns implements base.LogicalPlan interface.
func (ds *DataSource) PruneColumns(parentUsedCols []*expression.Column, opt *optimizetrace.LogicalOptimizeOp) (base.LogicalPlan, error) {
	used := expression.GetUsedList(ds.SCtx().GetExprCtx().GetEvalCtx(), parentUsedCols, ds.Schema())

	exprCols := expression.ExtractColumnsFromExpressions(nil, ds.AllConds, nil)
	exprUsed := expression.GetUsedList(ds.SCtx().GetExprCtx().GetEvalCtx(), exprCols, ds.Schema())
	prunedColumns := make([]*expression.Column, 0)

	originSchemaColumns := ds.Schema().Columns
	originColumns := ds.Columns

	ds.ColsRequiringFullLen = make([]*expression.Column, 0, len(used))
	for i, col := range ds.Schema().Columns {
		if used[i] || (ds.ContainExprPrefixUk && expression.GcColumnExprIsTidbShard(col.VirtualExpr)) {
			ds.ColsRequiringFullLen = append(ds.ColsRequiringFullLen, col)
		}
	}

	for i := len(used) - 1; i >= 0; i-- {
		if !used[i] && !exprUsed[i] {
			// If ds has a shard index, and the column is generated column by `tidb_shard()`
			// it can't prune the generated column of shard index
			if ds.ContainExprPrefixUk &&
				expression.GcColumnExprIsTidbShard(ds.Schema().Columns[i].VirtualExpr) {
				continue
			}
			prunedColumns = append(prunedColumns, ds.Schema().Columns[i])
			ds.Schema().Columns = append(ds.Schema().Columns[:i], ds.Schema().Columns[i+1:]...)
			ds.Columns = append(ds.Columns[:i], ds.Columns[i+1:]...)
		}
	}
	logicaltrace.AppendColumnPruneTraceStep(ds, prunedColumns, opt)
	addOneHandle := false
	// For SQL like `select 1 from t`, tikv's response will be empty if no column is in schema.
	// So we'll force to push one if schema doesn't have any column.
	if ds.Schema().Len() == 0 {
		var handleCol *expression.Column
		var handleColInfo *model.ColumnInfo
		handleCol, handleColInfo = preferKeyColumnFromTable(ds, originSchemaColumns, originColumns)
		ds.Columns = append(ds.Columns, handleColInfo)
		ds.Schema().Append(handleCol)
		addOneHandle = true
	}
	// ref: https://github.com/pingcap/tidb/issues/44579
	// when first entering columnPruner, we kept a column-a in datasource since upper agg function count(a) is used.
	//		then we mark the HandleCols as nil here.
	// when second entering columnPruner, the count(a) is eliminated since it always not null. we should fill another
	// 		extra col, in this way, handle col is useful again, otherwise, _tidb_rowid will be filled.
	if ds.HandleCols != nil && ds.HandleCols.IsInt() && ds.Schema().ColumnIndex(ds.HandleCols.GetCol(0)) == -1 {
		ds.HandleCols = nil
	}
	// Current DataSource operator contains all the filters on this table, and the columns used by these filters are always included
	// in the output schema. Even if they are not needed by DataSource's parent operator. Thus add a projection here to prune useless columns
	// Limit to MPP tasks, because TiKV can't benefit from this now(projection can't be pushed down to TiKV now).
	if !addOneHandle && ds.Schema().Len() > len(parentUsedCols) && ds.SCtx().GetSessionVars().IsMPPEnforced() && ds.TableInfo.TiFlashReplica != nil {
		proj := LogicalProjection{
			Exprs: expression.Column2Exprs(parentUsedCols),
		}.Init(ds.SCtx(), ds.QueryBlockOffset())
		proj.SetStats(ds.StatsInfo())
		proj.SetSchema(expression.NewSchema(parentUsedCols...))
		proj.SetChildren(ds)
		return proj, nil
	}
	return ds, nil
}

func (p *LogicalJoin) extractUsedCols(parentUsedCols []*expression.Column) (leftCols []*expression.Column, rightCols []*expression.Column) {
	for _, eqCond := range p.EqualConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(eqCond)...)
	}
	for _, leftCond := range p.LeftConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(leftCond)...)
	}
	for _, rightCond := range p.RightConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(rightCond)...)
	}
	for _, otherCond := range p.OtherConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(otherCond)...)
	}
	for _, naeqCond := range p.NAEQConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(naeqCond)...)
	}
	lChild := p.Children()[0]
	rChild := p.Children()[1]
	for _, col := range parentUsedCols {
		if lChild.Schema().Contains(col) {
			leftCols = append(leftCols, col)
		} else if rChild.Schema().Contains(col) {
			rightCols = append(rightCols, col)
		}
	}
	return leftCols, rightCols
}

func (p *LogicalJoin) mergeSchema() {
	p.SetSchema(buildLogicalJoinSchema(p.JoinType, p))
}

// PruneColumns implements base.LogicalPlan interface.
func (p *LogicalJoin) PruneColumns(parentUsedCols []*expression.Column, opt *optimizetrace.LogicalOptimizeOp) (base.LogicalPlan, error) {
	leftCols, rightCols := p.extractUsedCols(parentUsedCols)

	var err error
	p.Children()[0], err = p.Children()[0].PruneColumns(leftCols, opt)
	if err != nil {
		return nil, err
	}
	addConstOneForEmptyProjection(p.Children()[0])

	p.Children()[1], err = p.Children()[1].PruneColumns(rightCols, opt)
	if err != nil {
		return nil, err
	}
	addConstOneForEmptyProjection(p.Children()[1])

	p.mergeSchema()
	if p.JoinType == LeftOuterSemiJoin || p.JoinType == AntiLeftOuterSemiJoin {
		joinCol := p.Schema().Columns[len(p.Schema().Columns)-1]
		parentUsedCols = append(parentUsedCols, joinCol)
	}
	p.InlineProjection(parentUsedCols, opt)
	return p, nil
}

// PruneColumns implements base.LogicalPlan interface.
func (la *LogicalApply) PruneColumns(parentUsedCols []*expression.Column, opt *optimizetrace.LogicalOptimizeOp) (base.LogicalPlan, error) {
	leftCols, rightCols := la.extractUsedCols(parentUsedCols)
	allowEliminateApply := fixcontrol.GetBoolWithDefault(la.SCtx().GetSessionVars().GetOptimizerFixControlMap(), fixcontrol.Fix45822, true)
	var err error
	if allowEliminateApply && rightCols == nil && la.JoinType == LeftOuterJoin {
		logicaltrace.ApplyEliminateTraceStep(la.Children()[1], opt)
		resultPlan := la.Children()[0]
		// reEnter the new child's column pruning, returning child[0] as a new child here.
		return resultPlan.PruneColumns(parentUsedCols, opt)
	}

	// column pruning for child-1.
	la.Children()[1], err = la.Children()[1].PruneColumns(rightCols, opt)
	if err != nil {
		return nil, err
	}
	addConstOneForEmptyProjection(la.Children()[1])

	la.CorCols = coreusage.ExtractCorColumnsBySchema4LogicalPlan(la.Children()[1], la.Children()[0].Schema())
	for _, col := range la.CorCols {
		leftCols = append(leftCols, &col.Column)
	}

	// column pruning for child-0.
	la.Children()[0], err = la.Children()[0].PruneColumns(leftCols, opt)
	if err != nil {
		return nil, err
	}
	addConstOneForEmptyProjection(la.Children()[0])
	la.mergeSchema()
	return la, nil
}

func (*columnPruner) name() string {
	return "column_prune"
}

// By add const one, we can avoid empty Projection is eliminated.
// Because in some cases, Projectoin cannot be eliminated even its output is empty.
func addConstOneForEmptyProjection(p base.LogicalPlan) {
	proj, ok := p.(*LogicalProjection)
	if !ok {
		return
	}
	if proj.Schema().Len() != 0 {
		return
	}

	constOne := expression.NewOne()
	proj.Schema().Append(&expression.Column{
		UniqueID: proj.SCtx().GetSessionVars().AllocPlanColumnID(),
		RetType:  constOne.GetType(p.SCtx().GetExprCtx().GetEvalCtx()),
	})
	proj.Exprs = append(proj.Exprs, &expression.Constant{
		Value:   constOne.Value,
		RetType: constOne.GetType(p.SCtx().GetExprCtx().GetEvalCtx()),
	})
}

func preferKeyColumnFromTable(dataSource *DataSource, originColumns []*expression.Column,
	originSchemaColumns []*model.ColumnInfo) (*expression.Column, *model.ColumnInfo) {
	var resultColumnInfo *model.ColumnInfo
	var resultColumn *expression.Column
	if dataSource.table.Type().IsClusterTable() && len(originColumns) > 0 {
		// use the first column.
		resultColumnInfo = originSchemaColumns[0]
		resultColumn = originColumns[0]
	} else {
		if dataSource.HandleCols != nil {
			resultColumn = dataSource.HandleCols.GetCol(0)
			resultColumnInfo = resultColumn.ToInfo()
		} else if dataSource.table.Meta().PKIsHandle {
			// dataSource.HandleCols = nil doesn't mean datasource doesn't have a intPk handle.
			// since datasource.HandleCols will be cleared in the first columnPruner.
			resultColumn = dataSource.UnMutableHandleCols.GetCol(0)
			resultColumnInfo = resultColumn.ToInfo()
		} else {
			resultColumn = dataSource.newExtraHandleSchemaCol()
			resultColumnInfo = model.NewExtraHandleColInfo()
		}
	}
	return resultColumn, resultColumnInfo
}

// PruneColumns implements the interface of base.LogicalPlan.
// LogicalCTE just do an empty function call. It's logical optimize is indivisual phase.
func (p *LogicalCTE) PruneColumns(_ []*expression.Column, _ *optimizetrace.LogicalOptimizeOp) (base.LogicalPlan, error) {
	return p, nil
}

// PruneColumns implements the interface of base.LogicalPlan.
func (p *LogicalSequence) PruneColumns(parentUsedCols []*expression.Column, opt *optimizetrace.LogicalOptimizeOp) (base.LogicalPlan, error) {
	var err error
	p.Children()[p.ChildLen()-1], err = p.Children()[p.ChildLen()-1].PruneColumns(parentUsedCols, opt)
	if err != nil {
		return nil, err
	}
	return p, nil
}
