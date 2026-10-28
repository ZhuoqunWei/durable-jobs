"""Bounded synthetic reliability drills. Defaults to a plan, never to mutation.

Run on the private operator. Does not create cloud resources or delete volumes.
SQL faults target this runner's synthetic campaign IDs only and expire in 5 min.
The independent AWS cleanup timer is never modified.
"""
import argparse
import concurrent.futures
import fcntl
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re
import socket
import ssl
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

NS='durable-jobs-aws'
CONTEXT='arn:aws:eks:us-east-1:123456789012:cluster/durable-jobs-lab'
ORIGIN='https://jobs.durable-jobs-lab.internal'
SCENARIOS=['load','sustained-load','worker-crash','database-restart','slow-database','bad-deploy','poison-job','retry-exhaustion']


def validate_guard(context, namespace, run, cleanup_after, now, reserve_seconds):
    if context != CONTEXT or namespace != NS or not re.fullmatch(r'lab-\d{8}T\d{6}Z',run):
        raise ValueError('Not the approved private lab')
    if (datetime.fromisoformat(cleanup_after)-now).total_seconds() < reserve_seconds:
        raise ValueError('Insufficient time before automatic cleanup; refusing a new drill')


def percentile(values, p):
    return sorted(values)[max(0, math.ceil(len(values)*p)-1)] if values else None


def timestamp(value):
    # Go RFC3339Nano can have 1..9 fractional digits. Python 3.9 only accepts
    # selected widths with fromisoformat; normalize to microsecond precision.
    match=re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)',value)
    if not match: raise ValueError('Invalid RFC3339 timestamp')
    head,fraction,zone=match.groups()
    return datetime.fromisoformat(head+'.'+(fraction or '').ljust(6,'0')[:6]+('+00:00' if zone=='Z' else zone))


def acquire_run_lock(path):
    handle=open(path,'a')
    try: fcntl.flock(handle,fcntl.LOCK_EX|fcntl.LOCK_NB)
    except OSError:
        handle.close()
        raise RuntimeError('Another reliability drill is already running')
    return handle


def load_summary(records, elapsed):
    accepted=[r for r in records if 'job' in r]
    jobs=[r['job'] for r in accepted]
    def duration(job, key):
        return (timestamp(job[key])-timestamp(job['created_at'])).total_seconds()
    starts=[duration(j,'started_at') for j in jobs if j.get('started_at')]
    ends=[duration(j,'completed_at') for j in jobs if j.get('completed_at')]
    return {'submitted':len(records),'accepted':len(accepted),'request_errors':len(records)-len(accepted),
        'succeeded':sum(j['status']=='succeeded' for j in jobs), 'failed':sum(j['status']=='failed' for j in jobs),
        'unfinished':sum(j['status'] not in ('succeeded','failed') for j in jobs),
        'wall_seconds':round(elapsed,3),'completed_per_second':round(len(ends)/elapsed,3),
        'submit_seconds':{label:percentile([r['submit_seconds'] for r in records if 'submit_seconds' in r],p) for label,p in [('p50',.5),('p95',.95),('p99',.99)]},
        'start_seconds':{label:percentile(starts,p) for label,p in [('p50',.5),('p95',.95),('p99',.99)]},
        'completion_seconds':{label:percentile(ends,p) for label,p in [('p50',.5),('p95',.95),('p99',.99)]},
        'start_within_5s':sum(x<=5 for x in starts), 'terminal_within_60s':sum(x<=60 for x in ends),
        'slo_denominator':len(accepted)}


class Lab:
    def __init__(self,root):
        self.root=Path(root)
        self.session=json.loads((self.root/'session.json').read_text())
        self.prefix='rel_'+uuid.uuid4().hex[:12]
        self.ssl=ssl.create_default_context(cafile='/opt/durable-jobs/ca.crt')
        self.forwards=[]
        self.prom_port=None

    def guard(self,reserve_seconds=600):
        context=subprocess.run(['kubectl','config','current-context'],capture_output=True,text=True,check=True,timeout=15).stdout.strip()
        validate_guard(context,NS,self.session['run_id'],self.session['cleanup_after'],datetime.now(timezone.utc),reserve_seconds)
        namespace=self.get('namespace',NS)
        if namespace['metadata']['name'] != NS: raise ValueError('Namespace mismatch')

    def k(self,*args,body=None,timeout=35):
        command=['kubectl','--context',CONTEXT,'-n',NS,*args]
        result=subprocess.run(command,input=None if body is None else json.dumps(body),capture_output=True,text=True,timeout=timeout)
        if result.returncode:
            # Commands never request Secret contents or contain credentials.
            raise RuntimeError('kubectl '+str(args[:3])+': '+result.stderr[-1600:])
        return result.stdout

    def get(self,*args): return json.loads(self.k('get',*args,'-o','json'))

    def sql(self,statement):
        result=subprocess.run(['kubectl','--context',CONTEXT,'-n',NS,'exec','-i','postgres-0','--','psql','-U','postgres','-d','durable_jobs','-v','ON_ERROR_STOP=1','-At'],input=statement,text=True,capture_output=True,timeout=30)
        if result.returncode: raise RuntimeError('lab SQL failed: '+result.stderr[-1200:])
        return result.stdout.strip()

    def request(self,path,body=None,key=None,base=ORIGIN):
        headers={'Content-Type':'application/json'}
        if key: headers['Idempotency-Key']=key
        request=urllib.request.Request(base+path,data=None if body is None else json.dumps(body).encode(),headers=headers)
        opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPSHandler(context=self.ssl))
        with opener.open(request,timeout=5) as response:
            content=response.read(4*1024*1024)
            if path=='/metrics': return content.decode()
            return json.loads(content)

    def payload(self,suffix,invalid=False):
        return {'type':'migrate_campaign','payload':{'campaign_id':self.prefix+'_'+suffix,'category':'not-a-category' if invalid else 'Pets','created_at':'08/31/2026'}}

    def submit(self,suffix,invalid=False):
        return self.request('/jobs',self.payload(suffix,invalid),self.prefix+'-'+suffix)

    def wait(self,condition,seconds=90):
        end=time.monotonic()+seconds
        while time.monotonic()<end:
            try:
                value=condition()
                if value: return value
            except (urllib.error.URLError,TimeoutError,ConnectionError): pass
            time.sleep(.5)
        raise TimeoutError('Condition did not become true within '+str(seconds)+'s')

    def terminal(self,job,seconds=100):
        def check():
            value=self.request('/jobs/'+job['id'])
            return value if value['status'] in ('succeeded','failed') else None
        return self.wait(check,seconds)

    def history(self,job): return self.request('/jobs/'+job['id']+'/attempts')['attempts']

    def open_forward(self,service,remote):
        with socket.socket() as listener:
            listener.bind(('127.0.0.1',0))
            port=listener.getsockname()[1]
        process=subprocess.Popen(['kubectl','--context',CONTEXT,'-n',NS,'port-forward','svc/'+service,str(port)+':'+str(remote),'--address=127.0.0.1'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        self.forwards.append(process)
        origin='http://127.0.0.1:'+str(port)
        def ready():
            if process.poll() is not None: raise RuntimeError('Owned monitoring tunnel exited')
            return self.request('/api/v1/status/buildinfo' if service=='prometheus' else '/api/health',base=origin)
        self.wait(ready,15)
        if service=='prometheus': self.prom_port=port
        return origin

    def prom(self,path):
        if self.prom_port is None: raise RuntimeError('No owned Prometheus tunnel')
        return self.request(path,base='http://127.0.0.1:'+str(self.prom_port))

    def alerts(self): return self.prom('/api/v1/alerts')['data']['alerts']

    def wait_alert(self,name,seconds=60):
        return self.wait(lambda:[a for a in self.alerts() if a['labels']['alertname']==name and a['state']=='firing'],seconds)

    def install_faults(self):
        # No overwriting a pre-existing fault fixture: a previous failure needs review.
        self.sql("""BEGIN;
        CREATE TABLE lab_reliability_faults(campaign_id text PRIMARY KEY, delay_seconds double precision NOT NULL CHECK(delay_seconds BETWEEN 0 AND 10), expires_at timestamptz NOT NULL);
        GRANT SELECT ON lab_reliability_faults TO jobs_owner;
        CREATE FUNCTION lab_reliability_delay() RETURNS trigger LANGUAGE plpgsql AS $$
        DECLARE delay double precision;
        BEGIN
          IF OLD.status='running' AND NEW.status='succeeded' THEN
            SELECT delay_seconds INTO delay FROM lab_reliability_faults WHERE campaign_id=NEW.payload->>'campaign_id' AND expires_at>clock_timestamp();
            IF delay IS NOT NULL THEN PERFORM pg_sleep(delay); END IF;
          END IF;
          RETURN NEW;
        END $$;
        CREATE TRIGGER lab_reliability_delay BEFORE UPDATE OF status ON jobs FOR EACH ROW EXECUTE FUNCTION lab_reliability_delay();
        COMMIT;""")

    def fault(self,suffix,delay):
        if not re.fullmatch(r'[a-z0-9_]+',suffix) or not 0<=delay<=10: raise ValueError('Invalid fixture')
        self.sql("INSERT INTO lab_reliability_faults VALUES ('"+self.prefix+'_'+suffix+"',"+str(float(delay))+",clock_timestamp()+interval '5 minutes');")

    def clear_faults(self):
        self.sql('DROP TRIGGER IF EXISTS lab_reliability_delay ON jobs; DROP FUNCTION IF EXISTS lab_reliability_delay(); DROP TABLE IF EXISTS lab_reliability_faults;')

    def load(self,count=100,concurrency=8,rate=None):
        started=time.monotonic()
        def one(i):
            if rate:
                time.sleep(max(0,started+i/rate-time.monotonic()))
            before=time.monotonic()
            lag=before-(started+i/rate) if rate else 0
            suffix=('paced_' if rate else 'burst_')+str(i)
            try: return {'job':self.submit(suffix),'submit_seconds':time.monotonic()-before,'schedule_lag_seconds':lag}
            except Exception as exc: return {'error':type(exc).__name__,'submit_seconds':time.monotonic()-before,'key':self.prefix+'-'+suffix}
        with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool: records=list(pool.map(one,range(count)))
        completion_deadline=time.monotonic()+150
        for record in records:
            if 'job' in record:
                try: record['job']=self.terminal(record['job'],max(0,completion_deadline-time.monotonic()))
                except Exception as exc: record['completion_error']=type(exc).__name__
        (self.root/('load-records-'+self.prefix+'.json')).write_text(json.dumps(records,indent=2))
        summary=load_summary(records,time.monotonic()-started)
        if summary['request_errors'] or summary['failed'] or summary['unfinished']:
            (self.root/('failed-load-'+self.prefix+'.json')).write_text(json.dumps({'summary':summary,'records':records},indent=2))
            raise RuntimeError(json.dumps(summary))
        return {'parameters':{'count':count,'concurrency':concurrency,'worker_replicas':1,'target_requests_per_second':rate,
            'max_schedule_lag_seconds':max(r.get('schedule_lag_seconds',0) for r in records)},'summary':summary,'records':records}

    def sustained_load(self): return self.load(count=600,concurrency=8,rate=10)

    def worker_crash(self):
        self.install_faults()
        try:
            self.fault('crash',2.8)
            old=self.get('pods','-l','app=worker')['items'][0]
            job=self.submit('crash')
            self.wait(lambda:self.request('/jobs/'+job['id'])['status']=='running',10)
            # A committed running lease is sufficient evidence for kill-before-commit.
            started=time.monotonic()
            self.k('delete','pod',old['metadata']['name'],'--grace-period=0','--force','--wait=false')
            self.sql('DELETE FROM lab_reliability_faults;')
            result=self.terminal(job,90)
            history=self.history(job)
            assert result['status']=='succeeded' and result['attempts']==2
            assert [a['status'] for a in history]==['lease_expired','succeeded'],history
            self.k('rollout','status','deployment/worker','--timeout=60s',timeout=75)
            new=self.get('pods','-l','app=worker')['items'][0]
            return {'job':result,'history':history,'old_pod_uid':old['metadata']['uid'],'new_pod_uid':new['metadata']['uid'],'recovery_seconds':round(time.monotonic()-started,3),'injection':'kill after committed claim, before completion commit; external execution uniqueness not claimed'}
        finally: self.clear_faults()

    def database_restart(self):
        before=self.terminal(self.submit('db_before'))
        pvc=self.get('pvc','data-postgres-0')
        old=self.get('pod','postgres-0')
        started=time.monotonic()
        self.k('delete','pod','postgres-0','--wait=false')
        observations=[]
        end=time.monotonic()+150
        while time.monotonic()<end:
            try:
                ready=self.request('/readyz')
                observations.append({'seconds':round(time.monotonic()-started,3),'ready':True})
            except Exception as exc:
                observations.append({'seconds':round(time.monotonic()-started,3),'ready':False,'error':type(exc).__name__})
            current=self.get('pod','postgres-0') if self.k('get','pod','postgres-0','--ignore-not-found','-o','name').strip() else None
            if current and current['metadata']['uid']!=old['metadata']['uid'] and current.get('status',{}).get('containerStatuses',[{}])[0].get('ready') and observations[-1]['ready']: break
            time.sleep(1)
        else: raise TimeoutError('Postgres did not recover')
        after=self.terminal(self.submit('db_after'))
        replay=self.submit('db_before')
        assert replay['id']==before['id'] and replay['result']==before['result'] and replay['attempts']==1
        assert self.get('pvc','data-postgres-0')['spec']['volumeName']==pvc['spec']['volumeName']
        return {'recovery_seconds':round(time.monotonic()-started,3),'old_pod_uid':old['metadata']['uid'],'new_pod_uid':current['metadata']['uid'],'preserved_volume':pvc['spec']['volumeName'],'before':before,'after':after,'readiness_observations':observations}

    def slow_database(self):
        self.install_faults()
        try:
            for i in range(45): self.fault('slow_'+str(i),1.5)
            jobs=[self.submit('slow_'+str(i)) for i in range(45)]
            alert=self.wait_alert('JobQueueGrowing',65)
            latency_alert=self.wait_alert('JobCompletionLatencyHigh',65)
            results=[self.terminal(j,120) for j in jobs]
            assert all(j['status']=='succeeded' for j in results)
            self.wait(lambda:not [a for a in self.alerts() if a['labels']['alertname']=='JobQueueGrowing'],80)
            return {'injection':'1.5-second PostgreSQL completion-trigger delay; NOT a network proxy test','count':len(jobs),'alert_firing':alert,'latency_alert_firing':latency_alert,'queue_alert_resolved':True,'results':results}
        finally: self.clear_faults()

    def bad_deploy(self):
        template=self.get('deployment','api')['spec']['template']
        (self.root/'bad-deploy-original-template.json').write_text(json.dumps(template,indent=2)+'\n')
        started=time.monotonic()
        observations=[]
        try:
            self.k('patch','deployment','api','--type=strategic','-p',json.dumps({'spec':{'template':{'spec':{'containers':[{'name':'api','command':['/not-a-real-binary']}]}}}}))
            for _ in range(25):
                observations.append(self.request('/readyz'))
                pods=self.get('pods','-l','app=api')['items']
                broken=[p for p in pods if p['spec']['containers'][0].get('command')==['/not-a-real-binary'] and any(c.get('state',{}).get('waiting',{}).get('reason') in ('RunContainerError','CrashLoopBackOff','CreateContainerError') for c in p.get('status',{}).get('containerStatuses',[]))]
                if broken: break
                time.sleep(2)
            assert broken,'Crash was not observed'
            stalled=self.get('deployment','api')['status']
            assert stalled.get('updatedReplicas',0)==1 and stalled.get('availableReplicas',0)>=2,stalled
            evidence={'broken_pod':broken[0]['metadata']['name'],'bad_status':broken[0]['status']['containerStatuses'][0]['state'],'deployment_status':stalled}
        finally:
            # Replace template, rather than merging (which would retain the bad command).
            self.k('patch','deployment','api','--type=json','-p',json.dumps([{'op':'replace','path':'/spec/template','value':template}]))
            self.k('rollout','status','deployment/api','--timeout=120s',timeout=135)
        return {**evidence,'recovery_seconds':round(time.monotonic()-started,3),'healthy_requests_during_failure':len(observations),'automatic_runner_rollback':True,'github_cd_rollback_tested':False}

    def poison_job(self):
        results=[self.terminal(self.submit('poison_'+str(i),invalid=True)) for i in range(12)]
        assert all(j['status']=='failed' and j['failure_code']=='invalid_campaign' and j['attempts']==1 for j in results)
        alert=self.wait_alert('JobFailureRateHigh',60)
        good=self.terminal(self.submit('after_poison'))
        assert good['status']=='succeeded'
        return {'failed_jobs':results,'healthy_followup':good,'error_alert':alert,'policy':'permanent invalid payload is not retried; retained failed row, not a dedicated DLQ service'}

    def retry_exhaustion(self):
        self.install_faults()
        try:
            self.fault('exhaust',10)
            job=self.terminal(self.submit('exhaust'),125)
            history=self.history(job)
            assert job['status']=='failed' and job['attempts']==3 and job['failure_code']=='lease_expired'
            assert len(history)==3 and all(a['status']=='lease_expired' for a in history)
            return {'job':job,'history':history,'injection':'completion exceeds 3-second storage deadline on every attempt; three real 30-second leases expire'}
        finally: self.clear_faults()

    def close(self):
        for p in self.forwards:
            p.terminate()
            try: p.wait(timeout=5)
            except subprocess.TimeoutExpired: p.kill();p.wait()
        if getattr(self,'run_lock',None): self.run_lock.close()


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--execute',action='store_true')
    parser.add_argument('--scenario',choices=['all']+SCENARIOS,default='all')
    args=parser.parse_args()
    if not args.execute:
        print(json.dumps({'mode':'plan','scenarios':SCENARIOS,'namespace':NS,'new_cloud_resources':False,'deletes_volumes':False}))
        return
    lab=Lab(Path(__file__).resolve().parent)
    lab.run_lock=acquire_run_lock(lab.root/'runner.lock')
    lab.guard(900)
    report={'run_id':lab.session['run_id'],'started_at':datetime.now(timezone.utc).isoformat(),'results':[]}
    output=lab.root/('report-'+lab.prefix+'.json')
    try:
        lab.open_forward('prometheus',9090)
        report['prometheus_build']=lab.prom('/api/v1/status/buildinfo')
        report['targets']=lab.prom('/api/v1/targets')
        report['hardware']=lab.get('nodes')['items'][0]['status']['capacity']
        report['metrics_before']=lab.request('/metrics')
        for scenario in SCENARIOS if args.scenario=='all' else [args.scenario]:
            lab.guard(600)
            began=datetime.now(timezone.utc).isoformat()
            print(json.dumps({'starting':scenario,'at':began}),flush=True)
            try:
                result=getattr(lab,scenario.replace('-','_'))()
                record={'scenario':scenario,'status':'passed','started_at':began,'finished_at':datetime.now(timezone.utc).isoformat(),'evidence':result}
            except Exception as exc:
                record={'scenario':scenario,'status':'failed','started_at':began,'finished_at':datetime.now(timezone.utc).isoformat(),'error':str(exc)}
            report['results'].append(record)
            output.write_text(json.dumps(report,indent=2)+'\n')
            print(json.dumps({'scenario':scenario,'status':record['status'],'error':record.get('error')}),flush=True)
            if record['status']=='failed': break
        try:
            report['metrics_after']=lab.request('/metrics')
            report['alerts_after']=lab.alerts()
        except Exception as exc:
            report['final_observation_error']=type(exc).__name__+': '+str(exc)
        report['finished_at']=datetime.now(timezone.utc).isoformat()
        report['status']='passed' if all(r['status']=='passed' for r in report['results']) and 'final_observation_error' not in report else 'failed'
    finally:
        lab.close()
        output.write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps({'report':str(output),'status':report.get('status','interrupted')}),flush=True)
    if report['status']!='passed': raise SystemExit(1)


if __name__=='__main__': main()
