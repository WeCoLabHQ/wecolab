#!/usr/bin/env python3
"""No Docker calls: safety and artifact contract checks for disposable recovery runner."""
import json
import os
import pathlib
import stat
import tempfile
import unittest
from unittest.mock import patch

from recovery_runner import Refusal, Runner


class RecoverySafetyTests(unittest.TestCase):
    def test_only_named_scenarios_are_accepted(self):
        with self.assertRaisesRegex(Refusal, 'unknown scenario'):
            Runner('arbitrary-command')

    def test_default_lab_is_refused_before_docker_inspection(self):
        with patch.dict('os.environ', {'WECOLAB_DEV_PREFIX': '', 'WECOLAB_DEV_OWNER': ''}):
            r = Runner('recreate')
        with patch.object(r, 'docker', side_effect=AssertionError('lab inspected')):
            with self.assertRaisesRegex(Refusal, 'unique non-wcl'):
                r.preflight()

    def test_foreign_network_is_refused_before_api_mutation(self):
        with patch.dict('os.environ', {'WECOLAB_DEV_PREFIX': 'disposable',
                                    'WECOLAB_DEV_OWNER': 'disposableowner123',
                                    'WECOLAB_DEV_SUBNET': '198.19.0.0/24'}):
            r = Runner('recreate')
        foreign = json.dumps([{'Labels': {'wecolab.dev.owner':'another-owner'},
                               'IPAM': {'Config': [{'Subnet':'198.19.0.0/24'}]}}]).encode()
        with patch.object(r, 'docker', return_value=foreign):
            with self.assertRaisesRegex(Refusal, 'network is foreign'):
                r.preflight()

    def test_cache_owner_check_supports_daemon_path_translation(self):
        r = Runner('recreate')
        r.prefix, r.owner = 'disposable', 'disposableowner123'
        for root, cache_root in (('/srv/checkout', '/srv/checkout'),
                                 ('/host_mnt/Users/person/checkout', '/Users/person/checkout'),
                                 ('/run/desktop/mnt/host/c/checkout', '/run/desktop/mnt/host/c/checkout')):
            with self.subTest(root=root, cache_root=cache_root):
                mounts = [
                    {'Type':'bind', 'Destination':'/src', 'Source':root, 'RW':False},
                    {'Type':'bind', 'Destination':'/cache',
                     'Source':cache_root+'/hack/dev/cache/disposable', 'RW':True}]
                def docker(*args):
                    return b'43:1729\n43:1729\n' if args[2] == 'stat' else b'disposableowner123\n'
                with patch.object(r, 'docker', side_effect=docker):
                    self.assertTrue(r.cache_mount_owned('pub', {'Mounts':mounts}))

    def test_cache_owner_check_rejects_foreign_mounts_and_markers(self):
        r = Runner('recreate')
        r.prefix, r.owner = 'disposable', 'disposableowner123'
        source = {'Type':'bind', 'Destination':'/src', 'Source':'/srv/checkout', 'RW':False}
        cache = {'Type':'bind', 'Destination':'/cache',
                 'Source':'/srv/checkout/hack/dev/cache/disposable', 'RW':True}
        same = b'43:1729\n43:1729\n'
        cases = [
            ([source, cache], r.owner, b'43:1729\n43:9876\n'),
            ([source, cache], r.owner, b''),
            ([source, dict(cache, Type='volume')], r.owner, same),
            ([dict(source, RW=True), cache], r.owner, same),
            ([source, cache, dict(cache)], r.owner, same),
            ([source, cache], 'anotherowner456', same)]
        for mounts, marker, identities in cases:
            with self.subTest(mounts=mounts, marker=marker, identities=identities):
                def docker(*args):
                    return identities if args[2] == 'stat' else (marker+'\n').encode()
                with patch.object(r, 'docker', side_effect=docker):
                    self.assertFalse(r.cache_mount_owned('pub', {'Mounts':mounts}))

    def test_failed_readback_never_writes_passed_artifact(self):
        with tempfile.TemporaryDirectory() as root:
            r = Runner('recreate')
            r.directory = pathlib.Path(root)
            r.identity = {'ArchiveID':'id-0123','SystemID':'123','Timeline':1,
                          'BackupID':'20261006T100000','ExpectedAppSHA':'a'*40}
            r.checks = [{'Name':'database-readback','Passed':False,
                         'ExpectedSHA256':'a'*64,'ActualSHA256':'b'*64}]
            r.save('failed')
            result = json.loads((pathlib.Path(root)/'result.json').read_text())
            self.assertEqual(result['Version'], 1)
            self.assertEqual(result['Outcome'], 'failed')
            self.assertEqual(result['ExpectedAppSHA'], 'a'*40)
            self.assertFalse(result['Checks'][0]['Passed'])

    def test_unexpected_failure_retains_failed_artifact_and_propagates(self):
        with tempfile.TemporaryDirectory() as root:
            r = Runner('recreate')
            r.directory = pathlib.Path(root)/'run'
            with patch.object(r, 'preflight', side_effect=RuntimeError('unexpected failure')):
                with self.assertRaisesRegex(RuntimeError, 'unexpected failure'):
                    r.execute()
            result_path = r.directory/'result.json'
            self.assertTrue(result_path.is_file(), 'unexpected failure lost its evidence')
            self.assertEqual(json.loads(result_path.read_text())['Outcome'], 'failed')

    def test_provider_requires_unexpired_retention_before_delete(self):
        future = '2999-01-01T00:00:00Z'
        for deadline in ('2000-01-01T00:00:00Z', None, '2999-01-01T00:00:00', 'invalid', future):
            with self.subTest(deadline=deadline):
                r = Runner('provider-contract')
                replies = {
                    'get-object-lock-configuration': {'ObjectLockConfiguration': {'ObjectLockEnabled': 'Enabled'}},
                    'get-bucket-versioning': {'Status': 'Enabled'},
                    'list-object-versions': {'Versions': [{'Key': 'owned/backup.info', 'VersionId': 'v1'}]},
                    'get-object-retention': {'Retention': {'Mode': 'COMPLIANCE', 'RetainUntilDate': deadline}},
                }
                class Response:
                    returncode, stderr = 0, b''
                    def __init__(self, body):
                        self.stdout = json.dumps(body).encode()
                def signed(operation, *args, **kwargs):
                    if operation == 'delete-object':
                        self.assertEqual(deadline, future, 'expired or unproven retention reached destructive probe')
                        response = Response({})
                        response.returncode, response.stderr = 1, b'AccessDenied'
                        return response
                    if operation == 'head-object':
                        return Response({})
                    return Response(replies[operation])
                r.signed_s3 = signed
                r.console = r.grant = r.deploy = r.backup = lambda *args, **kwargs: None
                r.sentinel = lambda: b'1|original'
                r.metadata = lambda: {'MetadataKey': 'owned/backup.info'}
                readbacks = []
                r.restored = readbacks.append
                values = {'WECOLAB_AUTHORIZED_PROVIDER': '1', 'WECOLAB_PROVIDER_BUCKET': 'owned-bucket',
                          'WECOLAB_PROVIDER_ENDPOINT': 'https://provider.invalid',
                          'WECOLAB_PROVIDER_KEY_ID': 'read-key', 'WECOLAB_PROVIDER_SECRET': 'fixture',
                          'WECOLAB_PROVIDER_DENIED_KEY_ID': 'denied-key', 'WECOLAB_PROVIDER_DENIED_SECRET': 'fixture'}
                with patch.dict(os.environ, values):
                    if deadline == future:
                        r.provider_contract()
                        self.assertEqual(readbacks, [b'1|original'])
                    else:
                        with self.assertRaisesRegex(Refusal, 'retention'):
                            r.provider_contract()

    def test_provider_credentials_are_excluded_from_operation_log(self):
        with tempfile.TemporaryDirectory() as root:
            docker = pathlib.Path(root)/'docker'
            docker.write_text('#!/usr/bin/env python3\n'
                              'import sys\n'
                              'sys.stdin.read()\n'
                              'sys.stdout.write("{}\\n200")\n')
            docker.chmod(stat.S_IRUSR|stat.S_IWUSR|stat.S_IXUSR)
            r = Runner('recreate')
            with patch.dict(os.environ, {'PATH':root+os.pathsep+os.environ['PATH']}):
                r.console('POST', '/api/deploy', {'Name':'fixture','Vault':{'KeyID':'fixture','Key':'sensitive'}})
            self.assertNotIn('sensitive', '\n'.join(r.operations))
            self.assertIn('[redacted]', '\n'.join(r.operations))

    def test_legacy_archive_uses_database_identity_and_existing_generation(self):
        r = Runner('upgrade')
        r.current = lambda name=None, site='pub': {'Database':'rec-db','Archive':{'pub':3}}
        class Listing:
            returncode = 0
            stdout = (b'{"Contents":[{"Key":"dev/rec/rec-db-pub-g3/base/BACKUP/backup.info"},'
                      b'{"Key":"dev/other/rec-db-pub-g3/base/ZZZ/backup.info"},'
                      b'{"Key":"dev/rec/rec-db-pub-g3/base/nested/ZZZ/backup.info"}]}')
        class Metadata:
            returncode = 0
            stdout = b'server_name=cloud\nstatus=DONE\nsystemid=321\ntimeline=2\nend_time=2026-10-06T10:00:00Z\n'
        calls = []
        def signed(operation, *args):
            calls.append((operation,args))
            return Listing() if operation == 'list-objects-v2' else Metadata()
        r.signed_s3 = signed
        r.app = 'rec'
        result = r.metadata()
        self.assertEqual(result['ArchiveName'], 'rec-db-pub-g3')
        self.assertEqual(result['BackupID'], 'BACKUP')
        self.assertEqual(calls[0][1][-1], 'dev/rec/rec-db-pub-g3/base/')

    def test_backup_metadata_prefers_completion_time_over_lexical_key_order(self):
        r = Runner('failed-backup')
        r.app = 'rec'
        r.current = lambda name=None, site='pub': {'Database':'rec-db','ArchiveID':'rec-db','Archive':None}
        prefix = 'dev/rec/rec-db-pub/base/'
        class Result:
            returncode = 0
            def __init__(self, stdout):
                self.stdout = stdout
        def signed(operation, *args):
            if operation == 'list-objects-v2':
                return Result(json.dumps({'Contents':[{'Key':prefix+bid+'/backup.info'} for bid in ('z-old','a-new')]}).encode())
            bid = args[args.index('--key')+1].split('/')[-2]
            completed = '2026-10-06T10:00:00Z' if bid == 'z-old' else '2026-10-06T11:00:00Z'
            return Result(f'server_name=cloud\nstatus=DONE\nsystemid=321\ntimeline=2\nend_time={completed}\n'.encode())
        r.signed_s3 = signed
        self.assertEqual(r.metadata()['BackupID'], 'a-new')

    def test_partition_rejects_lost_or_changed_original_rows(self):
        expected = b'\n'.join(f'{n}|original'.encode() for n in range(1,257))
        promoted = b'\n258|promoted'
        extra = b'\n257|still-writable'
        cases = [
            (expected+promoted, True),
            (expected+extra+promoted, True),
            (expected.replace(b'1|original',b'1|changed',1)+promoted, False),
            (b'\n'.join(expected.splitlines()[1:])+extra+promoted, False)]
        for recovered, valid in cases:
            with self.subTest(valid=valid, rows=len(recovered.splitlines())):
                r = Runner('partition')
                r.deploy = lambda: None
                r.sentinel = lambda: expected
                r.backup = lambda **kw: None
                r.restored = lambda *args, **kw: None
                r.console = lambda *args, **kw: {'PreviousPrimary':'pub'}
                r.docker = lambda *args, **kw: b'false'
                r.fence_database = lambda site: 'old-cluster'
                r.resource = lambda *args, **kw: {'metadata':{'uid':'new-cluster'}}
                def wait(description, fn, seconds=900):
                    result = fn()
                    r.require(result, 'fixture condition failed: '+description)
                    return result
                r.wait = wait
                def sql(site, name, query):
                    if 'must-refuse' in query:
                        raise Refusal('read-only standby')
                    if 'pg_is_in_recovery' in query:
                        return b't' if site == 'pub' else b'f'
                    if query.startswith('INSERT'):
                        return b''
                    if query.startswith('SELECT marker'):
                        return b'still-writable'
                    if query.startswith('SELECT n,marker'):
                        return recovered
                    raise AssertionError('unexpected fixture query: '+query)
                r.sql = sql
                if valid:
                    r.partition()
                else:
                    with self.assertRaisesRegex(Refusal, 'committed'):
                        r.partition()

    def test_partition_stops_rejoin_if_old_fence_disappears(self):
        r = Runner('partition')
        r.prefix = 'fixture'
        running = {'pub': True}
        clusters = iter([{'metadata': {'uid': 'old-cluster'}},
                         {'metadata': {'uid': 'new-cluster'}}])
        r.deploy = lambda: None
        r.sentinel = lambda: b'1|original'
        r.backup = lambda **kw: None
        r.restored = lambda *args, **kw: None
        r.console = lambda *args, **kw: {'PreviousPrimary': 'pub'}
        r.fence_database = lambda site: 'old-cluster'
        r.resource = lambda *args, **kw: next(clusters)
        def docker(*args, **kw):
            if args[0] in ('stop', 'start'):
                running['pub'] = args[0] == 'start'
            return str(running['pub']).lower().encode()
        r.docker = docker
        def wait(description, fn, seconds=900):
            for _ in range(3):
                if result := fn():
                    return result
            raise Refusal('fixture timed out')
        r.wait = wait
        def sql(site, name, query):
            if 'must-refuse' in query:
                raise Refusal('read-only standby')
            if 'pg_is_in_recovery' in query:
                return b't' if site == 'pub' else b'f'
            if 'SELECT marker' in query:
                return b'still-writable'
            return b'1|original\n258|promoted'
        r.sql = sql
        with self.assertRaisesRegex(Refusal, 'fence'):
            r.partition()
        self.assertFalse(running['pub'], 'unsafe old incarnation must remain powered off')

    def test_flux_fence_loss_cannot_be_hidden_by_later_reapplication(self):
        r = Runner('failed-backup')
        r.prefix = 'fixture'
        running = {'pub': True}
        requested = []
        clusters = iter([
            {'metadata': {'uid': 'old-cluster'}},
            {'metadata': {'uid': 'old-cluster', 'annotations': {'cnpg.io/fencedInstances': '["*"]'}},
             'status': {'currentPrimary': 'old-instance'}},
        ])
        def resource(site, kind, *args):
            if kind == 'kustomization':
                return {'status': {'lastHandledReconcileAt': requested[0],
                                   'conditions': [{'type': 'Ready', 'status': 'True'}]}}
            return next(clusters)
        def kubectl(site, *args, **kw):
            if 'annotate' in args:
                requested[:] = [next(a.split('=', 1)[1] for a in args if a.startswith('reconcile.fluxcd.io/requestedAt='))]
            return b''
        def docker(*args, **kw):
            if args == ('stop', 'fixture-pub'):
                running['pub'] = False
                return b''
            raise AssertionError('unexpected container operation')
        r.resource, r.kubectl, r.docker = resource, kubectl, docker
        with patch('recovery_runner.time.sleep'), self.assertRaisesRegex(Refusal, 'fence'):
            r.reconcile_database_fence('pub', 'old-cluster')
        self.assertFalse(running['pub'], 'a lost fence must leave the old site powered off')

    def test_replay_lag_refuses_permanently_unknown_measurements(self):
        r = Runner('replay-lag')
        r.prefix = 'fixture'
        state = {'vault':True, 'promoted':False, 'rows':256}
        r.deploy = lambda: None
        r.sentinel = lambda: b'1|original'
        r.backup = lambda **kw: None
        r.restored = lambda *args, **kw: None
        r.dbpod = lambda site: 'replica'
        r.fence_database = lambda site: 'old-cluster'
        r.resource = lambda *args, **kw: {'metadata':{'uid':'new-cluster'}}
        r.current = lambda: {'Protection':{'RecoveryPoint':{
            'State':'unknown','Reason':'Exporter observation failed'},
            'Database':{'State':'protected' if state['vault'] else 'unknown'}}}
        def wait(description, fn, seconds=900):
            result = fn()
            r.require(result, 'no observation: '+description)
            return result
        r.wait = wait
        def docker(*args, **kw):
            if args[0] in ('stop','start') and args[1] == 'fixture-vault':
                state['vault'] = args[0] == 'start'
            return b''
        r.docker = docker
        def console(method, path, *args, **kw):
            if method == 'POST':
                state['promoted'] = True
            return {'PreviousPrimary':'pub'}
        r.console = console
        def sql(site, name, query, **kw):
            if query.startswith('INSERT'):
                state['rows'] += 1
                return b''
            if 'pg_is_in_recovery' in query:
                return b'f' if site == 'home' and state['promoted'] else b't'
            if 'count(*)' in query:
                return str(state['rows']).encode()
            if 'timeline_id' in query:
                return b'2' if state['promoted'] else b'1'
            return b'0/123'
        r.sql = sql
        with self.assertRaisesRegex(Refusal, 'measured healthy recovery'):
            r.replay_lag()

    def test_recipe_rollout_rejects_old_ready_replicaset(self):
        recipe = {'image': 'main:new', 'sidecars': [{'image': 'side:new'}]}
        cases = [
            ({'observedGeneration': 2, 'updatedReplicas': 1, 'readyReplicas': 1, 'replicas': 1}, False),
            ({'observedGeneration': 3, 'updatedReplicas': 0, 'readyReplicas': 1, 'replicas': 1}, False),
            ({'observedGeneration': 3, 'updatedReplicas': 1, 'readyReplicas': 1, 'replicas': 2}, False),
            ({'observedGeneration': 3, 'updatedReplicas': 1, 'readyReplicas': 1, 'replicas': 1}, True),
        ]
        for status, accepted in cases:
            with self.subTest(status=status):
                r = Runner('recipe-lifecycle')
                r.current = lambda: {'Workload': 'recipe'}
                r.resource = lambda *args: {
                    'metadata': {'generation': 3}, 'status': status,
                    'spec': {'replicas': 1, 'template': {'spec': {
                        'containers': [{'image': 'main:new'}, {'image': 'side:new'}]}}}}
                def wait(description, observe, seconds):
                    result = observe()
                    r.require(result, 'recipe rollout incomplete')
                    return result
                r.wait = wait
                if accepted:
                    self.assertEqual(r.recipe_rollout(recipe)['metadata']['generation'], 3)
                else:
                    with self.assertRaisesRegex(Refusal, 'rollout'):
                        r.recipe_rollout(recipe)

    def test_recipe_upgrade_waits_for_new_template_even_with_same_images(self):
        r = Runner('recipe-lifecycle')
        recipe = {'image': 'main:fixed', 'sidecars': [{'image': 'side:fixed'}]}
        previous = {'metadata': {'uid': 'deployment', 'generation': 3}}
        deployment = {'metadata': {'uid': 'deployment', 'generation': 3},
            'status': {'observedGeneration': 3, 'updatedReplicas': 1, 'readyReplicas': 1, 'replicas': 1},
            'spec': {'replicas': 1, 'template': {'spec': {
                'containers': [{'image': 'main:fixed'}, {'image': 'side:fixed'}]}}}}
        r.current = lambda: {'Workload': 'recipe'}
        r.resource = lambda *args: deployment
        def wait(description, observe, seconds):
            result = observe()
            r.require(result, 'recipe rollout incomplete')
            return result
        r.wait = wait
        with self.assertRaisesRegex(Refusal, 'rollout'):
            r.recipe_rollout(recipe, previous=previous)
        deployment['metadata']['generation'] = 4
        deployment['status']['observedGeneration'] = 4
        self.assertEqual(r.recipe_rollout(recipe, previous=previous)['metadata']['generation'], 4)

    def test_recipe_probe_cannot_fake_database_write_with_stdout(self):
        r = Runner('recipe-lifecycle')
        recipe = {'image': 'main:new', 'sidecars': [{'image': 'side:new'}]}
        r.recipe_rollout = lambda *args: {'spec': {'selector': {'matchLabels': {'app': 'recipe'}},
            'template': {'spec': {'containers': [{'name': 'main', 'image': 'main:new'}, {'name': 'side', 'image': 'side:new'}]}}}}
        r.sql = lambda *args: b'1|original'
        r.resource = lambda *args: {'status': {'nodeInfo': {'architecture': 'arm64'}}}
        def kubectl(site, *args, **kwargs):
            if 'get' in args:
                return json.dumps({'items': [{'metadata': {'name': 'recipe-new'},
                    'spec': {'nodeName': 'node', 'containers': [{'name': 'main', 'image': 'main:new'}, {'name': 'side', 'image': 'side:new'}]},
                    'status': {'conditions': [{'type': 'Ready', 'status': 'True'}],
                               'containerStatuses': [{'name': 'main', 'imageID': 'main-digest'}, {'name': 'side', 'imageID': 'side-digest'}]}}]}).encode()
            row, marker = args[-2:]
            return f'{row}|{marker}\n1|original'.encode()
        r.kubectl = kubectl
        with self.assertRaisesRegex(Refusal, 'database'):
            r.recipe_probe(recipe, b'exit 0', b'1|original', 'installed')


if __name__ == '__main__':
    unittest.main()
