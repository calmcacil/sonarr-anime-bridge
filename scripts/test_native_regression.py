import argparse
from datetime import datetime, timezone
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

import native_regression as native


FAKE_SERVER = r'''
import json, os, signal, sqlite3, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit
name = Path(sys.argv[0]).name
if '--version' in sys.argv:
    print('2.15.2' if name.startswith('ref') else 'working')
    sys.exit(0)
if 'early' in name:
    sys.exit(2)
print(json.dumps({'process_id':os.getpid()}),flush=True)
year = int(os.environ['PREWARM_YEARS'].split(',')[0])
db_path = os.environ['CACHE_DB_PATH']
class Handler(BaseHTTPRequestHandler):
    def handle_request(self):
        parsed = urlsplit(self.path)
        query = parse_qs(parsed.query)
        code, headers, body = 200, {}, b''
        if parsed.path.startswith('/cache/') and self.headers.get('Authorization') != 'Bearer ' + os.environ['ADMIN_TOKEN']:
            code = 404
        elif parsed.path == '/health':
            body = json.dumps({'status':'ok', 'checks':{'cache':{'status':'warming' if 'timeout' in name else 'ok'},'resolver':{'status':'ok'}}}).encode()
        elif parsed.path == '/cache/clear':
            if self.command != 'POST':
                code, headers = 405, {'Allow':'POST'}
            else:
                with sqlite3.connect(db_path) as db:
                    if 'clear' not in name: db.execute('DELETE FROM year_cache')
                body = b'{"status":"ok"}'
        elif parsed.path == '/cache/stats':
            with sqlite3.connect(db_path) as db: count = db.execute('SELECT COUNT(*) FROM year_cache').fetchone()[0]
            body = json.dumps({'entries':count}).encode()
        elif parsed.path == '/list':
            if self.command not in ('GET','HEAD'):
                code, headers = 405, {'Allow':'GET, HEAD'}
            elif query.get('season',['ALL'])[0] not in ('ALL','WINTER','SPRING','SUMMER','FALL') or query.get('category',['series'])[0] not in ('series','series-new') or not query.get('year',[str(year)])[0].isdigit():
                code = 400
            else:
                with sqlite3.connect(db_path) as db:
                    row = db.execute('SELECT data FROM year_cache WHERE year=?',(year,)).fetchone()
                rows = [{'tvdbId':show['id'], 'title':str(show['id'])} for show in json.loads(row[0])] if row else []
                if 'order' in name: rows.reverse()
                if 'title' in name: rows[0]['title'] = 'Changed'
                if 'empty' in name: rows = []
                body = json.dumps(rows).encode() if 'malformed' not in name else b'not json'
        else: code = 404
        self.send_response(code)
        for key,value in headers.items(): self.send_header(key,value)
        self.end_headers()
        if self.command != 'HEAD': self.wfile.write(body)
        if 'exit' in name and query.get('season') == ['FALL'] and query.get('category') == ['series-new']:
            threading.Thread(target=server.shutdown).start()
    do_GET = do_POST = do_HEAD = handle_request
    def log_message(self,*_): pass
server = ThreadingHTTPServer(('127.0.0.1',int(os.environ['PORT'])),Handler)
if 'fetch' in name: print(json.dumps({'task':'year_fetch','outcome':'started'}),flush=True)
def stop(*_):
    if 'shutdown' in name: os._exit(3)
    threading.Thread(target=server.shutdown).start()
signal.signal(signal.SIGTERM,stop)
server.serve_forever(poll_interval=.01)
server.server_close()
'''


class NativeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.year = datetime.now(timezone.utc).year - 1
        self.ref = self.binary('ref')
        self.snapshot = self.root / 'snapshot'
        full = json.dumps([{'id':1},{'id':2}]).encode()
        prior = json.dumps([{'id':3,'startDate':{'year':self.year-1,'month':12}},
                            {'id':4,'startDate':{'year':self.year-1,'month':11}}]).encode()
        native.save_snapshot(self.snapshot,self.year,full,prior,b'mapping',{'tag':'v2.15.2'})

    def binary(self,name):
        path = self.root / name
        path.write_text('#!' + sys.executable + '\n' + FAKE_SERVER)
        path.chmod(0o755)
        return path

    def execute(self,case='normal',*,extra=(),output='results',timeout='3'):
        work = self.binary('work-' + case)
        env = dict(os.environ, PATH=str(self.root), PORT='invalid', CACHE_DB_PATH='/wrong',
                   MAPPING_PATH='/wrong', ADMIN_TOKEN='wrong', PREWARM_YEARS='1900',
                   INCLUDE_TYPES='MOVIE', EXCLUDE_TAGS='Action', FILTER_FUTURE_ENABLED='false',
                   DEBUG_ENDPOINTS_ENABLED='false', NATIVE_REF_TAG='v2.15.2')
        return subprocess.run([sys.executable,str(native.ROOT/'scripts/native_regression.py'),
                               '--ref-binary',str(self.ref),'--work-binary',str(work),
                               '--snapshot',str(self.snapshot),'--output',str(self.root/output),
                               '--requests','2','--timeout',timeout,*extra],
                              env=env,text=True,capture_output=True,timeout=15)

    def test_native_offline_comparison_restart_metrics_and_environment(self):
        original = {p.name:p.read_bytes() for p in self.snapshot.iterdir()}
        result = self.execute()
        self.assertEqual(result.returncode,0,result.stderr+result.stdout)
        report = json.loads((self.root/'results/results.json').read_text())
        self.assertEqual(report['status'],'passed')
        self.assertEqual(report['year'],self.year)
        self.assertEqual(len(report['response_counts']),10)
        self.assertEqual(report['warm_http_timings']['working']['WINTER']['samples'],2)
        self.assertGreater(report['warm_http_timings']['working']['ALL']['median_ms'],0)
        self.assertEqual(len(list((self.root/'results').glob('*restart-*.json'))),20)
        self.assertTrue((self.root/'results/working.log').exists())
        self.assertEqual(original,{p.name:p.read_bytes() for p in self.snapshot.iterdir()})
        for path in report['runtime_directories']: self.assertFalse(Path(path).exists())
        # The process PATH has no Go, Git, gh, curl, or jq; replay needs only Python/binaries.
        self.assertIn('Results retained:',result.stdout)

    def test_order_titles_empty_malformed_clear_fetch_and_shutdown_fail(self):
        for case in ('order','title','empty','malformed','clear','fetch','shutdown'):
            with self.subTest(case=case):
                result = self.execute(case,output=case)
                self.assertNotEqual(result.returncode,0)
                report = json.loads((self.root/case/'results.json').read_text())
                self.assertEqual(report['status'],'failed')
                self.assertTrue(report['error'])
                for path in report.get('runtime_directories',[]): self.assertFalse(Path(path).exists())
                if case in ('order','title'): self.assertTrue((self.root/case/'working.diff').exists())

    def test_early_exit_and_readiness_timeout_are_bounded(self):
        for case in ('early','timeout'):
            with self.subTest(case=case):
                result = self.execute(case,output=case,timeout='.3')
                self.assertNotEqual(result.returncode,0)
                report = json.loads((self.root/case/'results.json').read_text())
                self.assertLess(report['elapsed_seconds'],5)
                self.assertIn('readiness',report['error'].lower())

    def test_existing_results_are_never_overwritten(self):
        output = self.root/'results'
        output.mkdir()
        original = b'previous report'
        (output/'results.json').write_bytes(original)
        result = self.execute()
        self.assertNotEqual(result.returncode,0)
        self.assertEqual((output/'results.json').read_bytes(),original)

    def test_successful_early_exit_cannot_pass_as_graceful_shutdown(self):
        result = self.execute('exit',extra=('--requests','0'))
        self.assertNotEqual(result.returncode,0)
        report = json.loads((self.root/'results/results.json').read_text())
        self.assertIn('before shutdown',report['error'])

    def test_cancellation_reaps_servers_and_preserves_report(self):
        output = self.root/'cancelled'
        work = self.binary('work-timeout')
        process = subprocess.Popen([sys.executable,str(native.ROOT/'scripts/native_regression.py'),
                                    '--ref-binary',str(self.ref),'--work-binary',str(work),
                                    '--snapshot',str(self.snapshot),'--output',str(output),
                                    '--timeout','10'],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
        self.addCleanup(lambda: process.kill() if process.poll() is None else None)
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            log = output/'working.log'
            if log.exists() and log.stat().st_size: break
            if process.poll() is not None: self.fail(process.communicate())
            time.sleep(.02)
        else:
            self.fail('Working server did not start')
        process.send_signal(signal.SIGTERM)
        process.communicate(timeout=5)
        self.assertNotEqual(process.returncode,0)
        report = json.loads((output/'results.json').read_text())
        self.assertEqual(report['status'],'failed')
        self.assertIn('Signal',report['error'])
        for path in output.glob('*.log'):
            for line in path.read_text().splitlines():
                pid = json.loads(line)['process_id']
                with self.assertRaises(ProcessLookupError): os.kill(pid,0)

    def test_bad_reference_version_and_snapshot_fail_before_replay(self):
        result = self.execute(extra=('--ref-tag','v2.15.1'))
        self.assertNotEqual(result.returncode,0)
        self.assertIn('expected v2.15.1',result.stderr)
        (self.snapshot/f'{self.year}.json').write_text('[]')
        result = self.execute(output='tampered')
        self.assertNotEqual(result.returncode,0)
        self.assertIn('checksum',result.stderr)

    def test_capture_cannot_pass_health_without_expected_year_rows(self):
        work = self.binary('work-normal')
        output = self.root/'capture-failure'
        result = subprocess.run([sys.executable,str(native.ROOT/'scripts/native_regression.py'),
                                 '--ref-binary',str(self.ref),'--work-binary',str(work),
                                 '--output',str(output),'--timeout','.3'],
                                text=True,capture_output=True,timeout=5)
        self.assertNotEqual(result.returncode,0)
        report = json.loads((output/'results.json').read_text())
        self.assertEqual(report['status'],'failed')
        self.assertIn('Readiness timed out',report['error'])
        self.assertTrue((output/'capture.log').exists())
        self.assertFalse((output/'snapshot').exists())

    def test_snapshot_december_integrity_and_nonempty_year(self):
        _, payloads = native.load_snapshot(self.snapshot,self.year)
        self.assertEqual([row['id'] for row in json.loads(payloads[f'{self.year-1}.json'])],[3])
        for raw in (b'[]',b'null',b'[null]',b'[{"id":true}]'):
            with self.subTest(raw=raw),self.assertRaises(ValueError):
                native.parse_shows(raw,nonempty=True)
        manifest = json.loads((self.snapshot/'manifest.json').read_text())
        manifest['files']['../outside'] = {}
        native.write_json(self.snapshot/'manifest.json',manifest)
        with self.assertRaises(ValueError): native.load_snapshot(self.snapshot,self.year)

    def test_mapping_replay_and_fresh_sqlite_seeding(self):
        _, payloads = native.load_snapshot(self.snapshot,self.year)
        with native.MappingReplay(payloads['mappings.json.zst']) as replay:
            for method in ('HEAD','GET'):
                with native.HTTP.open(native.Request(replay.url,method=method)) as response:
                    self.assertEqual(response.status,200)
                    self.assertEqual(response.headers['ETag'],'"snapshot"')
                    self.assertEqual(response.read(),b'mapping' if method=='GET' else b'')
            data = self.root/'data'
            native.seed_data(data,self.year,payloads,replay.url)
            rows = native.cached_years(data,self.year)
            self.assertEqual(rows[self.year],payloads[f'{self.year}.json'])
            self.assertEqual(rows[self.year-1],payloads[f'{self.year-1}.json'])
            with native.closing(native.sqlite3.connect(data/'cache.db')) as db:
                db.execute('UPDATE year_cache SET year=? WHERE year=?',(self.year-2,self.year-1))
                db.commit()
            self.assertIsNone(native.cached_years(data,self.year))

    def test_year_tag_and_sample_validation(self):
        self.assertEqual(native.arguments([]).year,self.year)
        self.assertEqual(native.arguments(['--snapshot',str(self.snapshot)]).year,self.year)
        for tag in ('main','v01.2.3','v2.1.0-rc.1','v2.1.0\n'):
            with self.subTest(tag=tag),self.assertRaises(ValueError): native.stable_tag(tag)
        for args in (['--year',str(self.year+1)],['--requests','-1'],['--timeout','nan']):
            with self.subTest(args=args),self.assertRaises(SystemExit): native.arguments(args)

    def test_release_cache_reuse_tamper_rebuild_and_build_failure(self):
        args = argparse.Namespace(ref_binary=None,ref_tag='v2.15.2',cache_dir=self.root/'cache',timeout=10)
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive,mode='w'): pass
        builds = []
        def run(command,**_):
            if command[:2] == ['git','rev-parse']: return 'a'*40
            if command[:2] == ['go','version']: return 'go version test'
            if command[:2] == ['go','env']: return 'linux\narm64'
            if command[:2] == ['go','build']:
                builds.append(command)
                path = Path(command[command.index('-o')+1])
                path.write_text('#!/bin/sh\nexit 0\n')
                path.chmod(0o755)
                return ''
            if command[-1] == '--version': return 'v2.15.2'
            raise AssertionError(command)
        with patch.object(native,'command',side_effect=run),patch.object(native.subprocess,'run',return_value=subprocess.CompletedProcess([],0,archive.getvalue())):
            for index in range(3):
                output = self.root/f'build-{index}'
                output.mkdir()
                binary,info = native.release_binary(args,output)
                if index == 1:
                    self.assertEqual(info['source'],'cached tag build')
                    binary.write_text('tampered')
            self.assertEqual(len(builds),2)
        with patch.object(native,'command',side_effect=RuntimeError('build failed')):
            with self.assertRaises(RuntimeError): native.work_binary(argparse.Namespace(work_binary=None,timeout=10),self.root)

    def test_make_checks_do_not_require_containers(self):
        makefile = (native.ROOT/'Makefile').read_text()
        checks = next(line for line in makefile.splitlines() if line.startswith('check:'))
        self.assertNotIn('docker',checks)
        self.assertIn('check-images: check docker-build',makefile)
        self.assertIn('regression:',makefile)


if __name__ == '__main__':
    unittest.main()
