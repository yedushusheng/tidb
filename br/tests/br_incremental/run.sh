#!/bin/sh
#
# Copyright 2019 PingCAP, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -eu
DB="$TEST_NAME"
TABLE="usertable"
AUTO_ID_TABLE="a"
ROW_COUNT=10
CUR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

run_sql "CREATE DATABASE $DB;"

go-ycsb load mysql -P $CUR/workload -p mysql.host=$TIDB_IP -p mysql.port=$TIDB_PORT -p mysql.user=root -p mysql.db=$DB
row_count_ori_full=$(run_sql "SELECT COUNT(*) FROM $DB.$TABLE;" | awk '/COUNT/{print $2}')

run_sql "CREATE TABLE IF NOT EXISTS ${DB}.${AUTO_ID_TABLE} (c1 INT);"
run_sql "create table ${DB}.t (pk bigint primary key, val int not null);"
run_sql "insert into ${DB}.t values (1, 11), (2, 22), (3, 33), (4, 44);"

echo "full backup start..."
run_br --pd $PD_ADDR backup db -s "local://$TEST_DIR/$DB/full" --db $DB

go-ycsb run mysql -P $CUR/workload -p mysql.host=$TIDB_IP -p mysql.port=$TIDB_PORT -p mysql.user=root -p mysql.db=$DB

for i in $(seq $ROW_COUNT); do
    run_sql "INSERT INTO ${DB}.${AUTO_ID_TABLE}(c1) VALUES ($i);"
done

run_sql "create index index_val on ${DB}.t (val);"
run_sql "update ${DB}.t set val = 66 - val;"

# incremental backup
echo "incremental backup start..."
last_backup_ts=$(run_br validate decode --field="end-version" -s "local://$TEST_DIR/$DB/full" | grep -oE "^[0-9]+")
run_br --pd $PD_ADDR backup db -s "local://$TEST_DIR/$DB/inc" --db $DB --lastbackupts $last_backup_ts
row_count_ori_inc=$(run_sql "SELECT COUNT(*) FROM $DB.$TABLE;" | awk '/COUNT/{print $2}')

run_sql "DROP DATABASE $DB;"

# full restore
echo "full restore start..."
run_br restore db --db $DB -s "local://$TEST_DIR/$DB/full" --pd $PD_ADDR
row_count_full=$(run_sql "SELECT COUNT(*) FROM $DB.$TABLE;" | awk '/COUNT/{print $2}')
# check full restore
if [ "${row_count_full}" != "${row_count_ori_full}" ];then
    echo "TEST: [$TEST_NAME] full restore fail on database $DB"
    exit 1
fi
# incremental restore
echo "incremental restore start..."
run_br restore db --db $DB -s "local://$TEST_DIR/$DB/inc" --pd $PD_ADDR
row_count_inc=$(run_sql "SELECT COUNT(*) FROM $DB.$TABLE;" | awk '/COUNT/{print $2}')
# check full restore
if [ "${row_count_inc}" != "${row_count_ori_inc}" ];then
    echo "TEST: [$TEST_NAME] incremental restore fail on database $DB"
    exit 1
fi

# check inserting records with auto increment id after incremental restore
for i in $(seq $ROW_COUNT); do
    run_sql "INSERT INTO ${DB}.${AUTO_ID_TABLE}(c1) VALUES ($i);"
done

if run_sql "admin check table ${DB}.t;" | grep -q 'inconsistency'; then
    echo "TEST: [$TEST_NAME] incremental restore fail on database $DB.t"
    exit 1
fi

index_count=$(run_sql "select count(*) from $DB.t use index (index_val);" | awk '/count/{print $2}')
if [ "${index_count}" != "4" ];then
    echo "TEST: [$TEST_NAME] index check fail on database $DB.t"
    exit 1
fi

run_sql "DROP DATABASE $DB;"
