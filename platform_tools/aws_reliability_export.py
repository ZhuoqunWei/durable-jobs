"""Collect non-secret evidence on the private operator; no cloud writes."""
import base64
from datetime import datetime, timezone
import gzip
import hashlib
import json
from pathlib import Path
import subprocess
import urllib.request

from aws_reliability import Lab


def main():
    lab=Lab(Path(__file__).resolve().parent)
    lab.guard(120)
    evidence={'collected_at':datetime.now(timezone.utc).isoformat()}
    try:
        grafana_origin=lab.open_forward('grafana',3000)
        evidence['grafana_health']=lab.request('/api/health',base=grafana_origin)
        password=base64.b64decode(lab.get('secret','grafana-admin')['data']['password']).decode()
        token=base64.b64encode(('admin:'+password).encode()).decode()
        request=urllib.request.Request(grafana_origin+'/api/dashboards/uid/durable-jobs-lab',headers={'Authorization':'Basic '+token})
        with urllib.request.urlopen(request,timeout=10) as response: dashboard=json.load(response)
        evidence['grafana_dashboard']={'uid':dashboard['dashboard']['uid'],'panels':len(dashboard['dashboard']['panels']),'provisioned':dashboard['meta'].get('provisioned'), 'title':dashboard['dashboard']['title']}
        evidence['pods']=[{'name':p['metadata']['name'],'uid':p['metadata']['uid'],'phase':p.get('status',{}).get('phase'),'containers':[{'name':c['name'],'ready':c['ready'],'restart_count':c['restartCount']} for c in p.get('status',{}).get('containerStatuses',[])]} for p in lab.get('pods')['items']]
        evidence['pvc']={k:lab.get('pvc','data-postgres-0')['spec'][k] for k in ['volumeName','storageClassName']}
        evidence['safe_worker_logs']=lab.k('logs','deployment/worker','--tail=160','--timestamps=true')
        evidence['safe_api_logs']=lab.k('logs','deployment/api','--tail=40','--timestamps=true')
        evidence['remaining_fault_tables']=lab.sql("SELECT count(*) FROM information_schema.tables WHERE table_name='lab_reliability_faults';")
        evidence['metrics']=lab.request('/metrics')
        evidence['readiness']=lab.request('/readyz')
        lab.open_forward('prometheus',9090)
        evidence['targets']=lab.prom('/api/v1/targets')
        evidence['alerts']=lab.alerts()
        for name in ['durable_jobs:slo_start_ratio','durable_jobs:slo_completion_ratio']:
            evidence[name]=lab.prom('/api/v1/query?query='+name)
        (lab.root/'final-evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
        reports=[p.name for p in lab.root.glob('report-*.json')]+['final-evidence.json']
        # Chunked export is read through SSM without granting any new S3 write role.
        for name in reports:
            packed=base64.b64encode(gzip.compress((lab.root/name).read_bytes(),mtime=0)).decode()
            (lab.root/(name+'.b64')).write_text(packed)
        print(json.dumps({'files':[{'name':name,'encoded_bytes':(lab.root/(name+'.b64')).stat().st_size,'sha256':hashlib.sha256((lab.root/name).read_bytes()).hexdigest()} for name in reports],
            'grafana':evidence['grafana_dashboard'],'readiness':evidence['readiness'],'remaining_fault_tables':evidence['remaining_fault_tables']}))
    finally: lab.close()


if __name__=='__main__': main()
