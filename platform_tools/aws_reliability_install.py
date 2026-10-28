"""On-operator installation for an explicitly authorized, unexpired lab only."""
import argparse
import copy
import json
from pathlib import Path
import secrets

from aws_reliability import Lab, NS


def install(lab):
    image = json.loads((lab.root/'image.json').read_text())["image"]
    if not image.startswith('123456789012.dkr.ecr.us-east-1.amazonaws.com/durable-jobs@sha256:'):
        raise ValueError('Expected pinned project image')
    # Completed migration Pods occupy scarce node slots; never touch the PVC.
    for job in lab.get('jobs')["items"]:
        if job['metadata']['name'].startswith('migrate-') and job.get('status', {}).get('succeeded') == 1:
            lab.k('delete', 'job', job['metadata']['name'], '--wait=true')
    original = {name: lab.get('deployment', name)['spec']['template'] for name in ['api','worker']}
    (lab.root/'original-templates.json').write_text(json.dumps(original))
    spec = copy.deepcopy(original['worker']['spec'])
    spec['restartPolicy'] = 'Never'
    spec['containers'][0].update(name='migrate', image=image, command=['/migrate'], args=[])
    for key in ['livenessProbe','readinessProbe','startupProbe','lifecycle']:
        spec['containers'][0].pop(key,None)
    job = {'apiVersion':'batch/v1','kind':'Job','metadata':{'name':'migrate-phase12','namespace':NS},
        'spec':{'backoffLimit':0,'activeDeadlineSeconds':120,'ttlSecondsAfterFinished':60,
                'template':{'metadata':{'labels':{'app':'migrate-phase12'}},'spec':spec}}}
    lab.k('create','-f','-',body=job)
    lab.k('wait','--for=condition=complete','job/migrate-phase12','--timeout=120s',timeout=135)
    lab.k('delete','job','migrate-phase12','--wait=true')
    try:
        for name in ['api','worker']:
            lab.k('set','image','deployment/'+name,name+'='+image)
            lab.k('rollout','status','deployment/'+name,'--timeout=180s',timeout=195)
        # Secret is generated on the private host; never emitted into output.
        found=lab.k('get','secret','grafana-admin','--ignore-not-found','-o','name')
        if not found.strip():
            lab.k('create','-f','-',body={'apiVersion':'v1','kind':'Secret','metadata':{'name':'grafana-admin','namespace':NS},
                'type':'Opaque','stringData':{'password':secrets.token_urlsafe(32)}})
        lab.k('apply','-f',str(lab.root/'monitoring.json'))
        for name in ['prometheus','grafana']:
            lab.k('rollout','status','deployment/'+name,'--timeout=300s',timeout=315)
    except Exception:
        for name, template in original.items():
            lab.k('patch','deployment',name,'--type=merge','-p',json.dumps({'spec':{'template':template}}))
        raise
    print(json.dumps({'status':'installed','image':image,'namespace':NS,'public_exposure_added':False}),flush=True)


if __name__=='__main__':
    parser=argparse.ArgumentParser()
    parser.add_argument('--execute',action='store_true')
    args=parser.parse_args()
    if not args.execute:
        print('Plan: migration, API/worker rollout, private Prometheus and Grafana; no new AWS resources')
    else:
        lab=Lab(Path(__file__).resolve().parent)
        lab.guard(reserve_seconds=1200)
        install(lab)
