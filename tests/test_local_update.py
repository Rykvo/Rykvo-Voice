import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import os
import socket
import threading
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch
import zipfile
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('updater', ROOT / 'deploy/local-update.py')
u = importlib.util.module_from_spec(spec)
spec.loader.exec_module(u)


class LocalUpdateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.state, self.live = self.root / 'state', self.root / 'live'
        self.state.mkdir(); self.live.mkdir()
        (self.live / 'VERSION').write_text('1.1.0')
        (self.live / 'UPDATE_EPOCH').write_text('2')
        for name, value in [('STATE', self.state), ('LIVE', self.live)]:
            p = patch.object(u, name, value); p.start(); self.addCleanup(p.stop)
        p = patch.object(u.platform, 'machine', return_value='x86_64'); p.start(); self.addCleanup(p.stop)

        self.key, self.private = os.urandom(32), os.urandom(32)
        self.public = Ed25519PrivateKey.from_private_bytes(self.private).public_key()
        for name, value in [('public_key', lambda: self.public), ('read_key', lambda: self.key)]:
            p = patch.object(u.crypto, name, value); p.start(); self.addCleanup(p.stop)
        self.owner = 'a' * 64

    def package(self, version='1.1.1', extra=None, manifest_edit=None, names_extra=False):
        files = {'VERSION': version.encode(), 'UPDATE_EPOCH': b'2', 'install.sh': b'#!/bin/bash\nexit 0', 'bin/rykvo-auth': b'binary', 'bin/cloudflared': b'binary', 'deploy/release.py': b'helper', 'deploy/local-update.py': b'helper', 'deploy/update_crypto.py': b'helper', 'deploy/update-signing.pub': b'public', 'deploy/rykvo-update-validate.service': b'unit', 'deploy/rykvo-update.socket': b'unit', 'deploy/rykvo-update.service': b'unit', 'deploy/rykvo-update@.service': b'unit', 'web/index.html': b'page', 'licenses/NOTICE': b'notice', 'manifest.json': b'{}'}
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w:gz') as tar:
            for name, content in files.items():
                member = tarfile.TarInfo(name); member.size = len(content)
                tar.addfile(member, io.BytesIO(content))
            if extra: tar.addfile(extra, io.BytesIO(b'x' * extra.size))
        payload = data.getvalue()
        m = {'schema': 1, 'product': 'rykvo-voice', 'epoch': 2, 'version': version, 'revision': 'a' * 40, 'assets': {arch: {'size': len(payload), 'sha256': hashlib.sha256(payload).hexdigest()} for arch in ('amd64', 'arm64')}}
        if manifest_edit: manifest_edit(m)
        out = io.BytesIO()
        with zipfile.ZipFile(out, 'w', compression=zipfile.ZIP_STORED) as z:
            z.writestr('release.json', json.dumps(m))
            for arch in ('amd64', 'arm64'): z.writestr(f'rykvo-voice-linux-{arch}.tar.gz', payload)
            if names_extra: z.writestr('../escape', 'invalid')
        source = self.root / 'plain.zip'; destination = self.root / 'test.rvu'
        source.write_bytes(out.getvalue())
        u.crypto.encrypt(source, destination, self.key, self.private)
        return destination.read_bytes()


    def start(self, data, owner=None):
        return u.query({'action':'start', 'owner':owner or self.owner, 'size':len(data), 'header':data[:28].hex(), 'seal':data[-96:].hex()}, None)['data']

    def chunk(self, state, data, offset=0, owner=None):
        return u.query({'action':'chunk','owner':owner or self.owner,'uploadId':state['uploadId'],'offset':offset,'size':len(data)},io.BytesIO(data))['data']

    def finish(self, state):
        with patch.object(u.subprocess,'run'):
            return u.query({'action':'finish','owner':self.owner,'uploadId':state['uploadId']},None)['data']

    def prepare(self, data=None):
        data = self.package() if data is None else data
        state = self.start(data)
        for offset in range(state['offset'],len(data),u.CHUNK_SIZE):
            state = self.chunk(state,data[offset:offset+u.CHUNK_SIZE],offset)
        self.finish(state); u.run(validating=True)
        result = u.status(self.owner)['data']
        if result['state']=='failed': raise ValueError(result['error'])
        return result

    def test_stage_then_explicit_apply_and_reverify(self):
        ready=self.prepare();self.assertEqual(ready['state'],'ready')
        self.assertEqual(ready['version'],'1.1.1')
        self.assertFalse((self.state/'verified.zip').exists())
        self.assertTrue((self.state/'verified.rvu').is_file())
        # Modifying extracted files never changes what is executed.
        (self.state/'staged/install.sh').write_text('malicious')
        with patch.object(u.subprocess,'run') as call:
            result=u.query({'action':'apply','owner':self.owner,'ticket':ready['ticket']},None)
            self.assertEqual(result['data']['state'],'queued')
            self.assertEqual(call.call_args.args[0],['systemctl','start','--no-block','rykvo-update.service'])
        def install(args,**kwargs):
            self.assertNotEqual((self.state/'staged/install.sh').read_text(),'malicious')
            return subprocess.CompletedProcess(args,0)
        with patch.object(u.subprocess,'run',side_effect=install):u.run()
        self.assertEqual(u.load()['state'],'complete')
        self.assertFalse((self.state/'staged').exists())
        self.assertFalse((self.state/'verified.rvu').exists())

    def test_unsigned_and_forged_seals_rejected_before_receiving_body(self):
        data=self.package()
        for bad in (b'MZ'+data[2:],data[:-1]+bytes([data[-1]^1]),b'PK'+b'0'*200):
            with self.assertRaises(ValueError): self.start(bad)
        self.assertFalse((self.state/'upload.part').exists())
        self.assertEqual(u.query({'action':'upload','owner':self.owner,'size':10},io.BytesIO(b'bad'))['error'],'INVALID_UPDATE_PACKAGE')

    def test_terminal_status_releases_session_ownership_without_exposing_tickets(self):
        for state in ('idle', 'complete', 'failed'):
            with self.subTest(state=state):
                data = {'state':state, 'owner':self.owner, 'version':'1.1.2',
                        'ticket':'private-ticket', 'uploadId':'private-upload', 'offset':128}
                if state == 'failed': data['error'] = 'UPDATE_INSTALL_FAILED'
                u.save(data)
                expected = {k:v for k,v in data.items() if k in ('state','version','error')}
                for owner in (self.owner, 'b'*64):
                    self.assertEqual(u.query({'action':'status','owner':owner},None), {'data':expected})
                self.assertEqual(u.load()['owner'], self.owner)
                self.assertEqual(u.query({'action':'apply','owner':'b'*64,'ticket':'private-ticket'},None), {'error':'UPDATE_CONFLICT'})

    def test_active_status_stays_owned_by_its_original_session(self):
        for state in ('uploading', 'validating', 'ready', 'queued', 'running'):
            with self.subTest(state=state):
                u.save({'state':state, 'owner':self.owner, 'ticket':'private-ticket'})
                self.assertEqual(u.query({'action':'status','owner':'b'*64},None), {'error':'UPDATE_BUSY'})
                self.assertEqual(u.status(self.owner)['data']['state'], state)

    def test_interrupted_install_reports_failure_to_a_new_session(self):
        for state in ('validating', 'queued', 'running'):
            with self.subTest(state=state), patch.object(u,'running',return_value=False):
                u.atomic_json(self.state/'status.json', {'state':state,'owner':self.owner,'updated':0,'ticket':'private-ticket'})
                self.assertEqual(u.query({'action':'status','owner':'b'*64},None),
                                 {'data':{'state':'failed','error':'UPDATE_INTERRUPTED'}})

    def test_completed_install_allows_new_session_upload(self):
        self.prepare()
        u.save(dict(u.load(),state='queued'))
        with patch.object(u.subprocess,'run',return_value=subprocess.CompletedProcess([],0)): u.run()
        self.assertEqual(u.query({'action':'status','owner':'b'*64},None)['data']['state'],'complete')
        self.assertEqual(self.start(self.package(),owner='b'*64)['state'],'uploading')

    def test_resume_duplicate_chunk_and_lost_ack(self):
        data=self.package();state=self.start(data);n=len(data)//2
        state=self.chunk(state,data[:n]);self.assertEqual(state['offset'],n)
        self.assertEqual(self.start(data)['offset'],n)
        self.assertEqual(self.chunk(state,data[:n])['offset'],n)
        with self.assertRaisesRegex(ValueError,'UPDATE_CONFLICT'): self.chunk(state,b'x'*n)
        state=self.chunk(state,data[n:],n);self.assertEqual(state['offset'],len(data))
        self.assertEqual((self.state/'upload.part').read_bytes(),data)

    def test_partial_chunk_not_committed_and_crash_tail_recovered(self):
        data=self.package();state=self.start(data)
        with self.assertRaisesRegex(ValueError,'UPDATE_CHUNK_INCOMPLETE'):
            u.query({'action':'chunk','owner':self.owner,'uploadId':state['uploadId'],'offset':0,'size':len(data)},io.BytesIO(data[:10]))
        self.assertEqual(u.transfer()['offset'],0)
        self.assertEqual((self.state/'upload.part').stat().st_size,0)
        (self.state/'upload.part').write_bytes(b'crash residue')
        self.start(data);self.assertEqual((self.state/'upload.part').stat().st_size,0)
        self.assertFalse((self.state/'chunk.tmp').exists())

    def test_wrong_order_owner_and_untrusted_fields_rejected(self):
        data=self.package();state=self.start(data)
        with self.assertRaises(ValueError):self.chunk(state,b'x',offset=1)
        with self.assertRaises(ValueError):self.chunk(state,b'x',owner='b'*64)
        with self.assertRaisesRegex(ValueError,'UPDATE_BUSY'):self.start(data,owner='b'*64)
        self.assertEqual(u.query({'action':'status','owner':'b'*64},None),{'error':'UPDATE_BUSY'})
        self.assertEqual(u.query({'action':'apply','owner':self.owner,'ticket':'x','command':'id'},None)['error'],'INVALID_UPDATE_PACKAGE')

    def test_finish_is_async_and_idempotent(self):
        data=self.package();state=self.start(data);self.chunk(state,data)
        with patch.object(u,'validate') as verify:
            self.assertEqual(self.finish(state)['state'],'validating');verify.assert_not_called()
        self.assertEqual(self.finish(state)['state'],'validating')
        u.run(validating=True);self.assertEqual(self.finish(state)['state'],'ready')

    def test_wrong_ciphertext_and_key_never_stage_or_execute(self):
        data=bytearray(self.package());data[40]^=1
        with self.assertRaisesRegex(ValueError,'UPDATE_SIGNATURE_INVALID'):self.prepare(bytes(data))
        with patch.object(u.crypto,'read_key',return_value=b'x'*32):
            with self.assertRaisesRegex(ValueError,'UPDATE_DECRYPT_FAILED'):self.prepare()
        self.assertFalse((self.state/'staged').exists())
        self.assertFalse((self.state/'verified.zip').exists())
        self.assertFalse((self.state/'verified.decrypting').exists())

    def test_rechecks_envelope_before_install(self):
        ready=self.prepare();blob=self.state/'verified.rvu';data=bytearray(blob.read_bytes());data[40]^=1;blob.write_bytes(data)
        u.save(dict(u.load(),state='queued'))
        with patch.object(u.subprocess,'run') as execute:u.run();execute.assert_not_called()
        self.assertEqual(u.load()['error'],'UPDATE_SIGNATURE_INVALID')

    def test_safe_extraction_and_inner_manifest(self):
        for name,kind in [('../outside',tarfile.REGTYPE),('/absolute',tarfile.REGTYPE),('web/link',tarfile.SYMTYPE),('VERSION',tarfile.REGTYPE)]:
            member=tarfile.TarInfo(name);member.type=kind
            with self.subTest(name=name),self.assertRaises(ValueError):self.prepare(self.package(extra=member))
        self.assertFalse((self.root/'outside').exists())
        for edit in [lambda m:m.update(product='bad'),lambda m:m.update(epoch=1),lambda m:m['assets']['arm64'].update(sha256='0'*64)]:
            with self.assertRaises(ValueError):self.prepare(self.package(manifest_edit=edit))

    def test_downgrade_and_legacy_epoch(self):
        with self.assertRaisesRegex(ValueError,'UPDATE_DOWNGRADE'):self.prepare(self.package('1.0.9'))
        (self.live/'VERSION').write_text('1.7.29');(self.live/'UPDATE_EPOCH').unlink()
        self.assertEqual(self.prepare()['state'],'ready')

    def test_full_package_skips_intermediate_versions_for_both_architectures(self):
        # Synthetic future version exercises the protocol, not a published release.
        for machine in ('x86_64', 'aarch64'):
            with self.subTest(machine=machine), patch.object(u.platform, 'machine', return_value=machine):
                (self.live/'VERSION').write_text('1.1.5')
                (self.live/'account-marker').write_text('preserve')
                ready = self.prepare(self.package('1.1.8'))
                self.assertEqual(ready['version'], '1.1.8')
                u.save(dict(u.load(), state='queued'))
                with patch.object(u.subprocess, 'run', return_value=subprocess.CompletedProcess([],0)) as install:
                    u.run()
                    self.assertEqual(install.call_args.args[0][-1], 'update')
                self.assertEqual(u.load()['state'], 'complete')
                self.assertEqual((self.live/'account-marker').read_text(), 'preserve')

    def test_skip_to_current_release_keeps_downgrade_protection(self):
        (self.live/'VERSION').write_text('1.1.1')
        self.assertEqual(self.prepare(self.package('1.1.5'))['state'], 'ready')
        (self.live/'VERSION').write_text('1.1.8')
        with self.assertRaisesRegex(ValueError, 'UPDATE_DOWNGRADE'):
            self.prepare(self.package('1.1.5'))

    def test_immutable_baseline_verifier_accepts_later_full_packages(self):
        spec = importlib.util.spec_from_file_location('compatibility', ROOT/'deploy/compatibility.py')
        compatibility = importlib.util.module_from_spec(spec); spec.loader.exec_module(compatibility)
        self.package('1.1.8')
        files = compatibility.baseline_files()
        from cryptography.hazmat.primitives import serialization
        files['update-signing.pub'] = self.public.public_bytes(
            serialization.Encoding.Raw, serialization.PublicFormat.Raw).hex().encode()
        with patch.object(compatibility, 'baseline_files', return_value=files):
            self.assertEqual(compatibility.verify(self.root/'test.rvu', self.key), '1.1.8')

    def test_baseline_checksum_is_required(self):
        spec = importlib.util.spec_from_file_location('compatibility', ROOT/'deploy/compatibility.py')
        compatibility = importlib.util.module_from_spec(spec); spec.loader.exec_module(compatibility)
        with patch.object(compatibility, 'BASELINE', self.root):
            (self.root/'SHA256.json').write_text(json.dumps({'bad': '0'*64}))
            with self.assertRaisesRegex(ValueError, 'baseline manifest'):
                compatibility.baseline_files()

    def test_quota_expiration_cancel_and_concurrency(self):
        data=self.package()
        with patch.object(u.shutil,'disk_usage',return_value=type('Usage',(),{'free':0})()):
            with self.assertRaisesRegex(ValueError,'UPDATE_STORAGE_LOW'):self.start(data)
        state=self.start(data)
        with (self.state/'lock').open('a') as lock:
            u.fcntl.flock(lock,u.fcntl.LOCK_EX)
            self.assertEqual(u.query({'action':'cancel','owner':self.owner,'uploadId':state['uploadId']},None)['error'],'UPDATE_BUSY')
        meta=u.transfer();meta['updated']=0;u.atomic_json(self.state/'transfer.json',meta)
        fresh=self.start(data);self.assertNotEqual(state['uploadId'],fresh['uploadId'])
        self.assertEqual(u.query({'action':'cancel','owner':self.owner,'uploadId':fresh['uploadId']},None)['data']['state'],'idle')
        self.assertFalse((self.state/'upload.part').exists())

    def test_chunk_broken_pipe_does_not_raise_or_lose_committed_offset(self):
        data=self.package();state=self.start(data)
        left,right=socket.socketpair();self.addCleanup(left.close);right.close()
        # A peer disappearing before a request is harmless and does not trigger a traceback.
        with patch.object(u.pwd,'getpwnam',return_value=type('User',(),{'pw_uid':os.getuid()})()):u.serve(left)
        self.assertEqual(u.transfer()['offset'],0)

    def test_socket_handshake_is_bounded_and_authenticated(self):
        data=self.package();state=self.start(data)
        left,right=socket.socketpair();self.addCleanup(left.close);self.addCleanup(right.close);right.settimeout(5)
        request={'action':'chunk','owner':self.owner,'uploadId':state['uploadId'],'offset':0,'size':len(data)}
        with patch.object(u.pwd,'getpwnam',return_value=type('User',(),{'pw_uid':os.getuid()})()):
            worker=threading.Thread(target=u.serve,args=(left,));worker.start()
            right.sendall((json.dumps(request)+'\n').encode())
            with right.makefile('rb') as response:
                self.assertEqual(json.loads(response.readline()),{'ready':True})
                right.sendall(data);self.assertEqual(json.loads(response.readline())['data']['offset'],len(data))
            worker.join(5);self.assertFalse(worker.is_alive())

if __name__=='__main__':unittest.main()
