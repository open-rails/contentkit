#!/usr/bin/env python3
"""Measure full UUID grouping/output against the already-created owned scalar DB."""
import argparse
import json
from pathlib import Path
import subprocess
import time
import uuid


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--metadata',type=Path,required=True)
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    meta=json.loads(args.metadata.read_text())
    db=meta['database']; container=meta['container']
    if not db.startswith('ck261_') or not db.replace('_','').isalnum():raise SystemExit('not an owned benchmark DB')
    args.output.mkdir(parents=True,exist_ok=False)
    records=[]
    def run(label,query):
        qid=db+'_uuid_'+label+'_'+uuid.uuid4().hex[:8]
        cmd=['docker','exec','-i',container,'bash','-c','exec clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" "$@"','ck261-uuid',
             '--max_threads','2','--max_insert_threads','2','--max_memory_usage','2147483648','--use_query_cache','0','--log_queries','1','--query_id',qid,'--format','JSONEachRow']
        start=time.monotonic(); out=subprocess.run(cmd,input=query.encode(),stdout=subprocess.PIPE,stderr=subprocess.PIPE,check=True)
        records.append({'label':label,'query_id':qid,'wall_seconds':time.monotonic()-start,'result_bytes':len(out.stdout)})
        (args.output/(label+'.sql')).write_text(query+'\n')
        (args.output/(label+'.jsonl')).write_bytes(out.stdout)
        return out.stdout
    run('create',f'''CREATE TABLE {db}.daily_uuid (scope UInt8,day Date,item_id UUID,qualified UInt32)
ENGINE=MergeTree PARTITION BY toYYYYMM(day) ORDER BY(scope,day,item_id)''')
    run('populate',f'''INSERT INTO {db}.daily_uuid SELECT scope,day,
toUUID(concat('00000000-0000-7000-8000-',leftPad(hex(item_id),12,'0'))),qualified
FROM {db}.daily WHERE scope=0 AND day<addDays(toDate('2025-01-01'),{meta['days']})''')
    for rep in range(3):
        for name,days in [('7d',7),('30d',30),('365d',365)]:
            rows=run(name+f'_run{rep+1}',f'''SELECT item_id,round(avg(toFloat64(qualified)/(eligible+20.0)),12) AS score
FROM {db}.daily_uuid INNER JOIN {db}.population USING(scope,day)
WHERE scope=0 AND day>=addDays(toDate('2025-01-01'),{meta['days']-days})
GROUP BY item_id ORDER BY score DESC,item_id ASC''')
            values=[json.loads(row) for row in rows.splitlines()]
            assert len(values)==meta['items'] and len({v['item_id'] for v in values})==meta['items']
            assert all((-a['score'],a['item_id'])<=(-b['score'],b['item_id']) for a,b in zip(values,values[1:]))
    run('flush','SYSTEM FLUSH LOGS')
    ids=','.join("'"+r['query_id']+"'" for r in records)
    metrics=run('metrics',f'''SELECT query_id,query_duration_ms,read_rows,read_bytes,result_rows,result_bytes,memory_usage,
ProfileEvents['OSReadBytes'] AS os_read_bytes,Settings['max_threads'] AS max_threads,exception_code
FROM system.query_log WHERE query_id IN ({ids}) AND type='QueryFinish' ORDER BY event_time_microseconds''')
    by_id={r['query_id']:r for r in (json.loads(x) for x in metrics.splitlines())}
    combined=[{**r,'server':by_id.get(r['query_id'])} for r in records]
    (args.output/'results.json').write_text(json.dumps(combined,indent=2)+'\n')
    print(json.dumps(combined,indent=2))

if __name__=='__main__':main()
