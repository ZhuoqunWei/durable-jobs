from datetime import datetime, timedelta, timezone
import unittest
import tempfile
from pathlib import Path
from unittest.mock import MagicMock, Mock, patch

from aws_reliability import CONTEXT, NS, Lab, acquire_run_lock, load_summary, percentile, timestamp, validate_guard


class ReliabilityTests(unittest.TestCase):
    def test_exited_tunnel_cannot_borrow_another_listener(self):
        lab=Lab.__new__(Lab)
        lab.forwards=[]
        lab.wait=lambda condition,seconds: condition()
        lab.request=Mock()
        process=Mock()
        process.poll.return_value=1
        with patch('aws_reliability.subprocess.Popen',return_value=process):
            with self.assertRaisesRegex(RuntimeError,'Owned monitoring tunnel exited'):
                lab.open_forward('prometheus',9090)
        lab.request.assert_not_called()

    def test_drills_cannot_overlap_and_lock_releases(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'runner.lock'
            with acquire_run_lock(path):
                with self.assertRaisesRegex(RuntimeError,'already running'): acquire_run_lock(path)
            with acquire_run_lock(path): pass

    def test_go_timestamp_widths_on_older_operator_python(self):
        for value,microseconds in [('2026-09-29T01:26:49.95148Z',951480),('2026-09-29T01:26:49.123456789+00:00',123456),('2026-09-29T01:26:49Z',0)]:
            self.assertEqual(timestamp(value).microsecond,microseconds)
        with self.assertRaises(ValueError): timestamp('2026-09-29')

    def test_guard_fails_closed(self):
        now=datetime(2026,9,29,tzinfo=timezone.utc)
        future=(now+timedelta(hours=1)).isoformat()
        validate_guard(CONTEXT,NS,'lab-20260928T210911Z',future,now,900)
        for context,namespace,run,expiry in [
            ('production',NS,'lab-20260928T210911Z',future),
            (CONTEXT,'default','lab-20260928T210911Z',future),
            (CONTEXT,NS,'anything',future),
            (CONTEXT,NS,'lab-20260928T210911Z',(now+timedelta(minutes=5)).isoformat()),
        ]:
            with self.assertRaises(ValueError): validate_guard(context,namespace,run,expiry,now,900)

    def test_load_denominator_includes_unfinished_and_failed(self):
        records=[{'error':'timeout'}, {'job':{'status':'queued','created_at':'2026-09-29T00:00:00Z'}},
            {'job':{'status':'failed','created_at':'2026-09-29T00:00:00Z','started_at':'2026-09-29T00:00:06Z','completed_at':'2026-09-29T00:01:01Z'}},
            {'job':{'status':'succeeded','created_at':'2026-09-29T00:00:00Z','started_at':'2026-09-29T00:00:01Z','completed_at':'2026-09-29T00:00:02Z'}}]
        s=load_summary(records,120)
        self.assertEqual(s['slo_denominator'],3)
        self.assertEqual(s['request_errors'],1)
        self.assertEqual(s['unfinished'],1)
        self.assertEqual(s['failed'],1)
        self.assertEqual(s['start_within_5s'],1)
        self.assertEqual(s['terminal_within_60s'],1)
        self.assertIsNone(percentile([], .95))
        self.assertEqual(percentile([3,1,2],.95),3)

    def test_bad_deploy_restores_template_after_failure(self):
        lab=Lab.__new__(Lab)
        lab.root=MagicMock()
        lab.get=Mock(return_value={'spec':{'template':{'metadata':{'labels':{'app':'api'}},'spec':{'containers':[{'name':'api','image':'known-good'}]}}}})
        lab.k=Mock(return_value='')
        lab.request=Mock(side_effect=RuntimeError('probe failed'))
        with self.assertRaisesRegex(RuntimeError,'probe failed'): lab.bad_deploy()
        restore=lab.k.call_args_list[-2].args
        self.assertIn('--type=json',restore)
        self.assertIn('"op": "replace"',restore[-1])
        self.assertIn('known-good',restore[-1])
        self.assertIn('rollout',lab.k.call_args_list[-1].args)

    def test_fault_payload_cannot_inject_sql(self):
        lab=Lab.__new__(Lab)
        lab.sql=Mock()
        with self.assertRaises(ValueError): lab.fault("a'; DROP TABLE jobs;",1)
        with self.assertRaises(ValueError): lab.fault('valid',11)
        lab.sql.assert_not_called()


if __name__=='__main__': unittest.main()
