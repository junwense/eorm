// Copyright 2021 ecodeclub
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aggregatemerger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	_ "unsafe"

	"github.com/ecodeclub/eorm/internal/rows"

	"github.com/ecodeclub/ekit/sqlx"

	"github.com/ecodeclub/eorm/internal/merger/internal/aggregatemerger/aggregator"
	"github.com/ecodeclub/eorm/internal/merger/internal/errs"
	"go.uber.org/multierr"
)

// Merger 该实现不支持group by操作,并且聚合函数查询应该只返回一行数据。
type Merger struct {
	aggregators []aggregator.Aggregator
	colNames    []string
	avgIndexes  []int
}

func NewMerger(aggregators ...aggregator.Aggregator) *Merger {
	cols := make([]string, 0, len(aggregators))
	idx := make([]int, 0, len(aggregators))
	for _, agg := range aggregators {
		info := agg.ColumnInfo()
		if agg.Name() == "AVG" {
			idx = append(idx, info.Index)
		}
		cols = append(cols, info.SelectName())
	}
	return &Merger{
		aggregators: aggregators,
		colNames:    cols,
		avgIndexes:  idx,
	}
}

func (m *Merger) Merge(ctx context.Context, results []rows.Rows) (rows.Rows, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(results) == 0 {
		return nil, errs.ErrMergerEmptyRows
	}
	for _, res := range results {
		if res == nil {
			return nil, errs.ErrMergerRowsIsNull
		}
	}

	return &Rows{
		rowsList:    results,
		aggregators: m.aggregators,
		avgIndexes:  m.avgIndexes,
		mu:          &sync.RWMutex{},
		// 原SQL中的聚合函数AVG传递到各个sql.Rows时会被转化为目标SQL中的AVG,SUM和COUNT三个列，这是一个对外不可见的转化。
		// 其中AVG仅用于获取ColumnType,真正的结果值是基于SUM和COUNT计算得到的
		// 所以设置aggregators要与目标SQL对齐, 而得到的merger.Rows应该与原SQL对齐的而不是目标SQL.
		columns: m.colNames,
	}, nil

}

type Rows struct {
	rowsList    []rows.Rows
	aggregators []aggregator.Aggregator
	avgIndexes  []int
	closed      bool
	mu          *sync.RWMutex
	lastErr     error
	cur         []any
	columns     []string
	nextCalled  bool
}

func (r *Rows) ColumnTypes() ([]*sql.ColumnType, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("%w", errs.ErrMergerRowsClosed)
	}
	ts, err := r.rowsList[0].ColumnTypes()
	if err != nil {
		return nil, err
	}
	if len(r.avgIndexes) == 0 {
		return ts, nil
	}
	v := make([]*sql.ColumnType, 0, len(ts))
	var prev int
	for i := 0; i < len(r.avgIndexes); i++ {
		idx := r.avgIndexes[i]
		v = append(v, ts[prev:idx+1]...)
		prev = idx + 3
	}
	return v, nil
}

func (*Rows) NextResultSet() bool {
	return false
}

func (r *Rows) Next() bool {
	r.mu.Lock()
	if r.closed || r.lastErr != nil {
		r.mu.Unlock()
		return false
	}
	if r.nextCalled {
		r.mu.Unlock()
		_ = r.Close()
		return false
	}

	rowsData, err := r.getSqlRowsData()
	r.nextCalled = true
	if err != nil {
		r.lastErr = err
		r.mu.Unlock()
		_ = r.Close()
		return false
	}
	// 进行聚合函数计算
	res, err := r.executeAggregateCalculation(rowsData)
	if err != nil {
		r.lastErr = err
		r.mu.Unlock()
		_ = r.Close()
		return false
	}
	r.cur = res
	r.mu.Unlock()
	return true

}

// getAggregateInfo 进行aggregate运算
func (r *Rows) executeAggregateCalculation(rowsData [][]any) ([]any, error) {
	res := make([]any, 0, len(r.aggregators))
	for _, agg := range r.aggregators {
		val, err := agg.Aggregate(rowsData)
		if err != nil {
			return nil, err
		}

		res = append(res, val)
	}
	return res, nil
}

// getSqlRowData 从sqlRows里面获取数据
func (r *Rows) getSqlRowsData() ([][]any, error) {
	// 所有sql.Rows的数据
	rowsData := make([][]any, 0, len(r.rowsList))
	for _, row := range r.rowsList {
		colData, err := r.getSqlRowData(row)
		if err != nil {
			return nil, err
		}
		rowsData = append(rowsData, colData)
	}
	return rowsData, nil
}

func (*Rows) getSqlRowData(row rows.Rows) ([]any, error) {
	var colsData []any
	var err error
	scanner, err := sqlx.NewSQLRowsScanner(row)
	if err != nil {
		return nil, err
	}
	colsData, err = scanner.Scan()
	if errors.Is(err, sqlx.ErrNoMoreRows) {
		return nil, errs.ErrMergerAggregateHasEmptyRows
	}
	return colsData, err
}

func (r *Rows) Scan(dest ...any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastErr != nil {
		return r.lastErr
	}
	if r.closed {
		return errs.ErrMergerRowsClosed
	}

	if len(r.cur) == 0 {
		return errs.ErrMergerScanNotNext
	}
	for i := 0; i < len(dest); i++ {

		err := rows.ConvertAssign(dest[i], r.cur[i])
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Rows) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	errorList := make([]error, 0, len(r.rowsList))
	for i := 0; i < len(r.rowsList); i++ {
		row := r.rowsList[i]
		err := row.Close()
		if err != nil {
			errorList = append(errorList, err)
		}
	}
	return multierr.Combine(errorList...)
}

func (r *Rows) Columns() ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, errs.ErrMergerRowsClosed
	}
	return r.columns, nil
}

func (r *Rows) Err() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastErr
}
