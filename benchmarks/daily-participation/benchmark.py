#!/usr/bin/env python3
"""Real ClickHouse benchmark of already-reduced daily item participation.

This is synthetic scalar aggregation, not receipt ingestion or source deduplication.
Uses an existing task-owned ClickHouse container and creates a unique database.
No services restart and no shared cache flush/drop.
"""
from __future__ import annotations
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import platform
import math
import struct
import subprocess
import time
import uuid

CLIENT = ['docker', 'exec', '-i']
ENTRY = ['bash', '-c', 'exec clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" "$@"', 'ck261-benchmark']


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--container', default='ck254-contracts-ch')
    p.add_argument('--items', type=int, default=100_000)
    p.add_argument('--days', type=int, default=365)
    p.add_argument('--scopes', type=int, default=1, choices=[1, 3])
    p.add_argument('--repeats', type=int, default=4)
    p.add_argument('--output', type=Path, required=True)
    args = p.parse_args()
    if not (1000 <= args.items <= 100_000 and 7 <= args.days <= 365):
        p.error('items must be 1000..100000 and days 7..365')
    args.output.mkdir(parents=True, exist_ok=False)
    db = 'ck261_' + dt.datetime.now(dt.UTC).strftime('%Y%m%d_%H%M%S') + '_' + uuid.uuid4().hex[:8]
    records = []
    query_ids = []

    def run(label, query, *, save=False, fmt='JSONEachRow'):
        qid = db + '_' + label + '_' + uuid.uuid4().hex[:8]
        cmd = CLIENT + [args.container] + ENTRY + [
            '--max_threads', '2', '--max_insert_threads', '2',
            '--max_memory_usage', '2147483648', '--log_queries', '1',
            '--use_query_cache', '0', '--query_id', qid,
            '--format', fmt,
        ]
        start = time.monotonic()
        completed = subprocess.run(cmd, input=query.encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        elapsed = time.monotonic() - start
        if completed.returncode:
            (args.output / (label + '.error.txt')).write_bytes(completed.stderr)
            raise RuntimeError(f'{label} failed, see {args.output / (label + ".error.txt")}')
        rec = {'label': label, 'query_id': qid, 'wall_seconds_including_docker_and_transport': elapsed,
               'result_bytes': len(completed.stdout), 'result_sha256': hashlib.sha256(completed.stdout).hexdigest()}
        records.append(rec)
        query_ids.append(qid)
        (args.output / 'client-receipts.json').write_text(json.dumps(records, indent=2) + '\n')
        (args.output / (label + '.sql')).write_text(query + '\n')
        if save:
            (args.output / (label + '.result')).write_bytes(completed.stdout)
        return completed.stdout

    version = run('server', 'SELECT version() AS version, timezone() AS timezone')
    metadata = {
        'database': db, 'container': args.container, 'items': args.items,
        'days': args.days, 'scopes': args.scopes, 'rows_expected': args.items * args.days * args.scopes,
        'rows_after_next_day_append': args.items * (args.days+1) * args.scopes,
        'daily_users': 60_000, 'distinct_items_per_user_day_assumption': 10,
        'qualified_item_user_days_assumption_per_year': 60_000 * 10 * args.days,
        'threads': 2, 'max_memory_bytes': 2_147_483_648, 'query_cache': False,
        'server': json.loads(version), 'python': platform.python_version(),
        'started_utc': dt.datetime.now(dt.UTC).isoformat(),
        'scope_populations': [60_000, 30_000, 15_000][:args.scopes],
        'pilot_scale_note': 'Tier geometry implies items*6 qualified item-days/day in scope0; the 60k*10 assumption matches only the full100k-item run.',
        'semantics': 'round(avg(U/(N+20)),12) over rows/days present; every synthetic item available every day',
        'limits': ['Daily scalars are synthetic; no people or raw receipt deduplication is executed.',
                   'Repeated visits are generated as excluded diagnostic counts, not added to U.',
                   'Three-scope mode uses overlapping synthetic audience subsets; it is not source eligibility proof.',
                   'First observed run follows insertion and is not a controlled cold-cache run.',
                   'No cache flushing or service restart; concurrent shared-host work may affect latency.',
                   'All-time equals available fixture history (365 days in full run).',
                   'No production scoring, erasure, rollout or current-algorithm parity claim.'],
    }
    (args.output / 'metadata.json').write_text(json.dumps(metadata, indent=2) + '\n')
    run('create_database', f'CREATE DATABASE {db}')
    run('create_daily', f'''CREATE TABLE {db}.daily
(scope UInt8, day Date, item_id UInt32, qualified UInt32, repeat_visits UInt32)
ENGINE=MergeTree PARTITION BY toYYYYMM(day) ORDER BY (scope,day,item_id)''')
    run('create_population', f'''CREATE TABLE {db}.population
(scope UInt8, day Date, eligible UInt32)
ENGINE=MergeTree ORDER BY (scope,day)''')
    run('populate_population', f'''INSERT INTO {db}.population
SELECT toUInt8(intDiv(number,{args.days})), addDays(toDate('2025-01-01'),number%{args.days}),
 toUInt32(60000 / pow(2,intDiv(number,{args.days}))) FROM numbers({args.days * args.scopes})''')
    # At 100k items the tier sums are exactly 600000 qualified people/items/day:
    # 100*1200 + 900*200 + 9000*20 + 30000*2 + 60000*1.
    # Top 1% gets half. Weekly rotation avoids an invariant leaderboard.
    count = args.items * args.days * args.scopes
    populate_query = f'''INSERT INTO {db}.daily
WITH toUInt8(intDiv(number,{args.items * args.days})) AS audience,
 intDiv(number%{args.items * args.days},{args.items}) AS d,
 toUInt32(number%{args.items}) AS item,
 (item + intDiv(d,7)*137)%{args.items} AS rank,
 multiIf(rank<{args.items}//1000,1200,rank<{args.items}//100,200,
 rank<{args.items}//10,20,rank<({args.items}*4)//10,2,1) AS raw_qualified,
 toUInt32(intDiv(raw_qualified,toUInt32(pow(2,audience)))) AS u
SELECT audience,addDays(toDate('2025-01-01'),d),item,u,
 toUInt32(u*(1+cityHash64(item,d)%4)) FROM numbers({count})'''.replace('//', '/')
    run('populate_daily', populate_query)
    run('dataset_check', f'''SELECT scope,count() AS rows,uniqExact(item_id) AS items,
 uniqExact(day) AS days,sum(qualified) AS qualified_item_days,sum(repeat_visits) AS repeat_visits
FROM {db}.daily GROUP BY scope ORDER BY scope''', save=True)
    # Let normal background merging settle without forcing a cluster-wide action.
    run('parts_before', f'''SELECT table,sum(rows) AS rows,sum(bytes_on_disk) AS bytes_on_disk,
 sum(data_uncompressed_bytes) AS uncompressed_bytes,count() AS parts
 FROM system.parts WHERE database='{db}' AND active GROUP BY table ORDER BY table''', save=True)
    windows = [('7d', min(7,args.days)), ('30d', min(30,args.days)), ('365d', args.days), ('all', args.days)]
    ranking_queries = {}
    for scope in range(args.scopes):
        for name,days in windows:
            lower = args.days - days
            query = f'''SELECT item_id,round(avg(toFloat64(qualified)/(eligible+20.0)),12) AS score
FROM {db}.daily INNER JOIN {db}.population USING (scope,day)
WHERE scope={scope} AND day>=addDays(toDate('2025-01-01'),{lower})
GROUP BY item_id ORDER BY score DESC,item_id ASC'''
            ranking_queries[f'scope{scope}_{name}'] = query
    for repeat in range(args.repeats):
        for name,query in ranking_queries.items():
            run(name + f'_run{repeat+1}', query, save=True, fmt='RowBinary')
    verification = []
    for name in ranking_queries:
        data = (args.output / (name + f'_run{args.repeats}.result')).read_bytes()
        decoded = list(struct.iter_unpack('<Id', data))
        if len(decoded) != args.items or len({row[0] for row in decoded}) != args.items:
            raise RuntimeError(f'{name}: result lost or duplicated item IDs')
        if any((-a[1], a[0]) > (-b[1], b[0]) for a,b in zip(decoded,decoded[1:])):
            raise RuntimeError(f'{name}: ordering violated score descending, ID ascending')
        scope = int(name.split('_')[0][5:])
        days = dict(windows)[name.split('_')[1]]
        scores = dict(decoded)
        for item in [0,1,99,100,999,1000,args.items//2,args.items-1]:
            expected = 0.0
            for d in range(args.days-days,args.days):
                rank = (item + (d//7)*137)%args.items
                raw = 1200 if rank<args.items/1000 else 200 if rank<args.items/100 else 20 if rank<args.items/10 else 2 if rank<args.items*4/10 else 1
                u = raw//(2**scope)
                expected += u/(60000/(2**scope)+20.0)
            expected /= days
            if not math.isclose(scores[item],expected,rel_tol=1e-11,abs_tol=5.1e-13):
                raise RuntimeError(f'{name}: sample scalar score mismatch for item {item}')
        hashes = {row['result_sha256'] for row in records if row['label'].startswith(name+'_run')}
        if len(hashes)!=1:
            raise RuntimeError(f'{name}: repeated ordering differs')
        verification.append({'bucket':name,'unique_sorted_ids':len(decoded),'sample_scores_checked':8,'all_repeats_identical':True})
    (args.output/'verification.json').write_text(json.dumps(verification,indent=2)+'\n')
    run('create_prepared', f'CREATE TABLE {db}.prepared (scope UInt8,bucket String,rank UInt32,item_id UInt32,score Float64) ENGINE=MergeTree ORDER BY(scope,bucket,rank)')
    for name, query in ranking_queries.items():
        scope = int(name.split('_')[0][5:])
        bucket = name.split('_')[1]
        run('persist_'+name, f"INSERT INTO {db}.prepared SELECT toUInt8({scope}),'{bucket}',toUInt32(row_number() OVER(ORDER BY score DESC,item_id ASC)),item_id,score FROM ({query})")
        run('page_'+name, f"SELECT item_id,score FROM {db}.prepared WHERE scope={scope} AND bucket='{bucket}' ORDER BY rank LIMIT 24",save=True,fmt='RowBinary')
    # Isolate aggregation without sorting/output by returning an aggregate checksum;
    # the all-ID result queries above are the actual prepared-ranking candidate.
    run('aggregate_without_sort', f'''SELECT count(),sum(score) FROM (
SELECT item_id,round(avg(toFloat64(qualified)/(eligible+20.0)),12) AS score
FROM {db}.daily INNER JOIN {db}.population USING(scope,day)
WHERE scope=0 GROUP BY item_id)''', save=True)
    # Cost of adding one day of ALREADY REDUCED scalars, not raw receipt reduction.
    next_query = populate_query.replace(f'intDiv(number,{args.items * args.days})', f'intDiv(number,{args.items})')
    next_query = next_query.replace(f'intDiv(number%{args.items * args.days},{args.items}) AS d', f'toUInt32({args.days}) AS d')
    next_query = next_query.replace(f'FROM numbers({count})', f'FROM numbers({args.items * args.scopes})')
    run('append_next_day_scalars', next_query)
    run('append_next_day_population', f"INSERT INTO {db}.population SELECT toUInt8(number),addDays(toDate('2025-01-01'),{args.days}),toUInt32(60000/pow(2,number)) FROM numbers({args.scopes})")
    run('create_prepared_next', f'CREATE TABLE {db}.prepared_next AS {db}.prepared')
    for name, query in ranking_queries.items():
        scope = int(name.split('_')[0][5:])
        bucket = name.split('_')[1]
        days = dict(windows)[bucket]
        old_lower = args.days - days
        lower = 0 if bucket=='all' else old_lower+1
        refreshed = query.replace(f"addDays(toDate('2025-01-01'),{old_lower})", f"addDays(toDate('2025-01-01'),{lower})")
        run('refresh_'+name, f"INSERT INTO {db}.prepared_next SELECT toUInt8({scope}),'{bucket}',toUInt32(row_number() OVER(ORDER BY score DESC,item_id ASC)),item_id,score FROM ({refreshed})")
    run('parts_after', f'''SELECT table,sum(rows) AS rows,sum(bytes_on_disk) AS bytes_on_disk,
 sum(data_uncompressed_bytes) AS uncompressed_bytes,count() AS parts
 FROM system.parts WHERE database='{db}' AND active GROUP BY table ORDER BY table''', save=True)
    # FLUSH LOGS writes diagnostics only; it does not flush data/OS/query caches.
    run('flush_query_logs', 'SYSTEM FLUSH LOGS')
    ids = ','.join("'"+x+"'" for x in query_ids)
    rows = run('query_metrics', f'''SELECT query_id,type,query_duration_ms,read_rows,read_bytes,
 written_rows,written_bytes,result_rows,result_bytes,memory_usage,
 ProfileEvents['UserTimeMicroseconds'] AS cpu_user_us,
 ProfileEvents['SystemTimeMicroseconds'] AS cpu_system_us,
 ProfileEvents['OSReadBytes'] AS os_read_bytes,
 ProfileEvents['ReadBufferFromFileDescriptorReadBytes'] AS filesystem_read_bytes,
 ProfileEvents['MarkCacheHits'] AS mark_cache_hits,ProfileEvents['MarkCacheMisses'] AS mark_cache_misses,
 Settings['max_threads'] AS max_threads,exception_code,exception FROM system.query_log
WHERE query_id IN ({ids}) AND type IN ('QueryFinish','ExceptionWhileProcessing','ExceptionBeforeStart')
ORDER BY event_time_microseconds''', save=True)
    metrics = [json.loads(line) for line in rows.splitlines()]
    by_id = {x['query_id']: x for x in metrics}
    combined = [{**x, 'server': by_id.get(x['query_id'])} for x in records]
    (args.output / 'results.json').write_text(json.dumps(combined, indent=2) + '\n')
    metadata['completed_utc'] = dt.datetime.now(dt.UTC).isoformat()
    (args.output / 'metadata.json').write_text(json.dumps(metadata, indent=2) + '\n')
    summary = []
    for row in combined:
        if row['label'].startswith('scope'):
            server = row['server'] or {}
            summary.append({'query': row['label'], 'ms': server.get('query_duration_ms'),
                            'wall_s': round(row['wall_seconds_including_docker_and_transport'],3),
                            'read_rows': server.get('read_rows'), 'read_bytes': server.get('read_bytes'),
                            'memory_bytes': server.get('memory_usage'), 'result_bytes': row['result_bytes']})
    print(json.dumps({'database':db,'output':str(args.output),'rankings':summary},indent=2))

if __name__ == '__main__':
    main()
