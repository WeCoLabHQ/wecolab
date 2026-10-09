#!/usr/bin/env python3
"""Disposable, named, evidence-producing CNPG recovery drills. No host kubectl context."""
import base64
import concurrent.futures
import contextlib
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import uuid

SCENARIOS = ('failed-backup', 'recreate', 'partition', 'replay-lag', 'rotation',
             'remote-storage', 'name-race', 'host-failure', 'provider-contract',
             'recipe-lifecycle', 'upgrade')
SHA = lambda data: hashlib.sha256(data).hexdigest()
NOW = lambda: dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds').replace('+00:00', 'Z')

class Refusal(Exception):
    pass

class Runner:
    def __init__(self, scenario):
        if scenario not in SCENARIOS:
            raise Refusal('unknown scenario; choose: ' + ', '.join(SCENARIOS))
        self.scenario = scenario
        self.prefix = os.getenv('WECOLAB_DEV_PREFIX', '')
        self.owner = os.getenv('WECOLAB_DEV_OWNER', '')
        self.network = os.getenv('WECOLAB_DEV_NETWORK', self.prefix + '-net')
        self.zone = os.getenv('WECOLAB_DEV_ZONE', 'dev.wecolab.test')
        self.subnet = os.getenv('WECOLAB_DEV_SUBNET', '198.18.0.0/24')
        self.runid = uuid.uuid4().hex[:12]
        self.app = 'rec-' + self.runid
        self.restore = 'read-' + self.runid
        self.ns = 'dev'
        self.started = NOW()
        self.operations = []
        self.checks = []
        self.versions = {}
        self.identity = {}
        self.scope = 'database'
        self.vault = {'Bucket':'wecolab-dev','Endpoint':'http://' + self.subnet.rsplit('.', 1)[0] + '.10',
                      'KeyID':'wecolab','Key':'wecolab-dev-vault'}
        base = os.getenv('WECOLAB_RECOVERY_ARTIFACTS', str(pathlib.Path(os.getenv('TMPDIR','/tmp'))/'wecolab-recovery-evidence'))
        self.directory = pathlib.Path(base) / (self.started.replace(':', '') + '-' + scenario + '-' + self.runid)

    def run(self, args, *, stdin=None, timeout=90, disclose=True):
        # Log exact argv except password-bearing APIs (log the operation shape only).
        display = ' '.join(args)
        if disclose:
            self.operations.append(display)
        else:
            self.operations.append(' '.join(args[:3]) + ' [redacted request body]')
        p = subprocess.run(args, input=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        if p.returncode:
            stderr = p.stderr.decode(errors='replace')
            raise Refusal(f'{args[0]} operation failed ({p.returncode}): {stderr[:300]}')
        return p.stdout

    def docker(self, *args, **kw):
        return self.run(['docker', *args], **kw)

    def box(self, site):
        assert site in ('pub', 'home', 'mac')
        return self.prefix + '-' + site

    def kubectl(self, site, *args, stdin=None, timeout=90):
        return self.docker('exec', '-i' if stdin is not None else self.box(site),
                           *((self.box(site),) if stdin is not None else ()),
                           'k3s', 'kubectl', *args, stdin=stdin, timeout=timeout)

    def resource(self, site, kind, name, ns=None):
        return json.loads(self.kubectl(site, '-n', ns or self.ns, 'get', kind, name, '-o', 'json'))

    def console(self, method, path, body=None, site='pub', allowed=(200, 201, 202), with_status=False):
        endpoint = 'https://127.0.0.1'
        if site != 'pub':
            interfaces = json.loads(self.docker('exec',self.box(site),'ip','-j','-4','addr','show','dev','nebula1'))
            addresses = [a['local'] for link in interfaces for a in link.get('addr_info',[]) if a['family'] == 'inet']
            self.require(len(addresses) == 1, 'private Console requires one site mesh address')
            endpoint = 'http://'+addresses[0]+':30800'
        argv = ['docker', 'exec', *(['-i'] if body is not None else []), self.box(site), 'curl', '-ksS', '-w', '\n%{http_code}',
                '-X', method, '-H', 'Host: console.' + self.zone,
                '-H', 'Content-Type: application/json']
        if body is not None:
            argv += ['--data-binary', '@-']
        argv += [endpoint + path]
        payload = json.dumps(body).encode() if body is not None else None
        if body is not None:
            def sanitize(value):
                if isinstance(value, dict):
                    return {k: ('[redacted]' if k.lower() in ('key','keyid','secret','password','token','invite','evidence') else sanitize(v)) for k,v in value.items()}
                if isinstance(value, list):
                    return [sanitize(x) for x in value]
                return value
            self.operations.append('Console '+method+' '+path+' '+json.dumps(sanitize(body),sort_keys=True))
        out = self.run(argv, stdin=payload, disclose=body is None)
        raw, code = out.rsplit(b'\n', 1)
        if int(code) not in allowed:
            raise Refusal(f'Console {method} {path}: HTTP {code.decode()}: {raw[:180].decode(errors="replace")}')
        try:
            response = json.loads(raw)
        except json.JSONDecodeError:
            response = raw.decode()
        return (int(code), response) if with_status else response

    def require(self, assertion, message):
        if not assertion:
            raise Refusal(message)

    def wait(self, description, fn, seconds=900):
        deadline = time.monotonic() + seconds
        last = None
        while time.monotonic() < deadline:
            try:
                result = fn()
                if result:
                    self.operations.append('observed ' + description)
                    return result
            except (Refusal, subprocess.TimeoutExpired, ValueError, KeyError) as err:
                last = str(err)
            time.sleep(5)
        raise Refusal(f'timed out waiting for {description}: {last or "no matching observation"}')

    def cache_mount_owned(self, site, box):
        # Docker Desktop may report aliases for the same bind source. Compare
        # the actual mounted directory with the read-only checkout, not path text.
        mounts = box.get('Mounts', [])
        roots = [m for m in mounts if m.get('Destination') == '/src']
        caches = [m for m in mounts if m.get('Destination') == '/cache']
        if len(roots) != 1 or len(caches) != 1:
            return False
        root, cache = roots[0], caches[0]
        if root.get('Type') != 'bind' or root.get('RW') is not False or cache.get('Type') != 'bind' or cache.get('RW') is not True:
            return False
        identities = self.docker('exec', self.box(site), 'stat', '-c', '%d:%i',
                                 '/cache', '/src/hack/dev/cache/'+self.prefix).splitlines()
        if len(identities) != 2 or identities[0] != identities[1]:
            return False
        return self.docker('exec', self.box(site), 'cat', '/cache/.wecolab-owner').strip().decode() == self.owner

    def preflight(self):
        self.require(re.fullmatch(r'[a-z][a-z0-9-]{0,30}', self.prefix) and self.prefix != 'wcl',
                     'choose a unique non-wcl WECOLAB_DEV_PREFIX; existing wcl-* are user data')
        self.require(re.fullmatch(r'[A-Za-z0-9_-]{12,80}', self.owner), 'WECOLAB_DEV_OWNER ownership token required')
        self.require(re.fullmatch(r'[a-z][a-z0-9-]{0,50}', self.network), 'invalid isolated network')
        self.require(re.fullmatch(r'(?:\d{1,3}\.){3}0/24', self.subnet) and self.subnet != '198.18.0.0/24',
                     'choose isolated /24 distinct from user wcl-net')
        self.require(self.network != 'wcl-net', 'shared wcl-net network forbidden')
        net=json.loads(self.docker('network','inspect',self.network))[0]
        self.require((net.get('Labels') or {}).get('wecolab.dev.owner') == self.owner and
                     self.subnet in [c.get('Subnet') for c in (net.get('IPAM') or {}).get('Config',[])],
                     'network is foreign or has a different subnet')
        cache=pathlib.Path('hack/dev/cache')/self.prefix
        self.require(cache.is_dir() and not cache.is_symlink() and
                     (cache/'.wecolab-owner').read_text().strip() == self.owner,
                     'cache is foreign or unlabelled')
        for site in ('pub','home','mac','vault'):
            name=self.prefix+'-'+site
            box=json.loads(self.docker('inspect',name))[0]
            self.require(((box.get('Config') or {}).get('Labels') or {}).get('wecolab.dev.owner') == self.owner,
                         'container is foreign or unlabelled: '+name)
            self.require(box.get('NetworkSettings',{}).get('Networks',{}).get(self.network) is not None,
                         'container is outside isolated network: '+name)
            volume=self.prefix+'-'+site+('-var' if site != 'vault' else '')
            self.require(any(m.get('Type') == 'volume' and m.get('Name') == volume for m in box.get('Mounts',[])),
                         'container has foreign persistent volume: '+name)
            self.require(self.docker('volume','inspect','-f','{{index .Labels "wecolab.dev.owner"}}',volume).strip().decode() == self.owner,
                         'volume is foreign or unlabelled: '+volume)
            if site != 'vault':
                self.require(self.cache_mount_owned(site, box), 'container has foreign cache mount: '+name)
                self.require(box.get('State',{}).get('Running'), 'fabric box stopped: '+site)
        # The public Door can still route to the previous writer after takeover.
        settings = self.console('GET', '/api/settings')
        self.require(settings.get('isWriter') and settings.get('site') == 'pub' and settings.get('writer') == 'pub',
                     'pub must be the disposable writer served by the public Console before starting')
        state = self.console('GET', '/api/state')
        self.require({x['Name'] for x in state['sites']} >= {'pub', 'home'}, 'pub/home sites missing')
        self.require(all(s['Ready'] and all(b['Ready'] for b in s['Boxes']) for s in state['sites'] if s['Name'] in ('pub','home')),
                     'disposable pub/home fabric boxes are not Ready')
        self.versions['docker'] = self.docker('version', '-f', '{{.Server.Version}}').strip().decode()
        self.versions['k3s'] = self.docker('exec', self.box('pub'), 'k3s', '--version').decode().splitlines()[0]
        self.versions['warden-sites'] = ','.join(
            site+':'+self.kubectl(site, '-n', 'wecolab-system', 'exec', 'deployment/wecolab-warden',
                                '--', '/proc/1/exe', 'version').strip().decode()
            for site in ('pub', 'home'))
        pods = json.loads(self.docker('exec',self.box('pub'),'k3s','kubectl','get','pods','-A','-o','json'))
        for component, fragment in [('cnpg','cloudnative-pg'),('barman','barman-cloud')]:
            images = {c['image'] for pod in pods['items'] for c in pod['spec'].get('containers',[])
                      if fragment in c['image']}
            self.require(images, 'component version unavailable: '+component)
            self.versions[component] = ','.join(sorted(images))

    def current(self, name=None, site='pub'):
        matches = [a for a in self.console('GET', '/api/state', site=site)['apps'] if a['Namespace'] == self.ns and a['Name'] == (name or self.app)]
        self.require(len(matches) == 1, 'app absent or ambiguous')
        return matches[0]


    def grant(self, project, site):
        state = self.console('GET', '/api/state')
        boxes = [b['Name'] for s in state['sites'] if s['Name'] == site for b in s['Boxes'] if not b['Laptop']]
        self.require(boxes, 'no non-laptop placement box at '+site)
        offer = 'rec-' + uuid.uuid4().hex[:12]
        self.console('POST', '/api/offers', {'Name':offer,'Site':site,'CPU':'4','Memory':'8Gi',
                                              'Storage':'40Gi','Boxes':boxes,'To':[project]})
        return offer
    def deploy(self, name=None, *, sites=('pub', 'home'), primary='pub', catalog=None, database=True, image='nginxinc/nginx-unprivileged:1.27-alpine'):
        name = name or self.app
        self.require(all(x['Name'] != name or x['Namespace'] != self.ns for x in self.console('GET', '/api/state')['apps']), 'fixture app name already exists')
        body = dict(Name=name, Project=self.ns, Sites=list(sites), Primary=primary, Database=database, RPO='5m')
        if catalog:
            body['Catalog'] = catalog
        else:
            body.update(Image=image, Port=8080)
        if database:
            body['Vault'] = self.vault.copy()
        self.console('POST', '/api/deploy', body)
        return self.wait('database-ready ' + name, lambda: (a if a['Ready'] == 'True' and a['Active'] == primary else None) if (a := self.current(name)) else None)

    def dbpod(self, site, name=None):
        name = name or self.app
        cluster = self.resource(site, 'cluster.postgresql.cnpg.io', name + '-db')
        pod = cluster.get('status', {}).get('currentPrimary')
        self.require(pod, f'CNPG primary not observed for {name} on {site}')
        return pod

    def sql(self, site, name, query, *, pod=None, database=None):
        pod = pod or self.dbpod(site, name)
        return self.kubectl(site, '-n', self.ns, 'exec', pod, '-c', 'postgres', '--', 'psql', '-X', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres', '-d', database or name, '-At', '-c', query).strip()

    def force_fenced(self, before, site='home'):
        self.require(self.docker('inspect','-f','{{.State.Running}}',self.box(before['PreviousPrimary'])).strip() == b'false',
                     'old primary is no longer physically fenced')
        # The pre-fault preview identifies what was fenced, not the new writer's revision.
        preview = self.console('GET',f'/api/apps/{self.ns}/{self.app}/force-preview',site=site)
        self.require(all(preview[key] == before[key] for key in ('ArchiveID','PreviousPrimary')),
                     'fenced app identity or previous primary changed before force')
        self.console('POST',f'/api/apps/{self.ns}/{self.app}/primary',
                     {'To':site,'Force':True,'Fencing':dict(preview,Method='power-off',
                      Evidence='Disposable old primary container stopped and inspected after writer takeover')},site=site)

    def sentinel(self, site='pub', name=None):
        name = name or self.app
        self.sql(site, name, 'CREATE TABLE IF NOT EXISTS public.wecolab_recovery (n bigint PRIMARY KEY, marker text NOT NULL)')
        marker = uuid.uuid4().hex
        self.sql(site, name, "INSERT INTO public.wecolab_recovery SELECT n, '" + marker + "' FROM generate_series(1,256) n")
        expected = self.sql(site, name, 'SELECT n,marker FROM public.wecolab_recovery ORDER BY n')
        self.require(len(expected.splitlines()) == 256, 'committed sentinel rows missing')
        return expected

    def fence_database(self, site):
        """Keep the old postmaster stopped across container/pod restart until Cluster replacement."""
        cluster = self.resource(site, 'cluster', self.app+'-db')
        uid = cluster['metadata']['uid']
        self.kubectl(site, '-n', self.ns, 'annotate', 'cluster', self.app+'-db',
                     'cnpg.io/fencedInstances=["*"]', '--overwrite',
                     '--field-manager=flux-client-side-apply')
        self.wait('old database process fenced before power-off',
                  lambda: self.database_fenced(site, uid), 180)
        self.reconcile_database_fence(site, uid)
        return uid

    def reconcile_database_fence(self, site, uid):
        """Reconcile desired roles without reapplying the independently established fence."""
        token = uuid.uuid4().hex
        kustomization = 'app-'+self.ns+'-'+self.app
        self.kubectl(site, '-n', 'flux-system', 'annotate', 'kustomization', kustomization,
                     'reconcile.fluxcd.io/requestedAt='+token, '--overwrite')
        def reconciled():
            status = self.resource(site, 'kustomization', kustomization, 'flux-system').get('status', {})
            return status.get('lastHandledReconcileAt') == token and any(
                c['type'] == 'Ready' and c['status'] == 'True' for c in status.get('conditions', []))
        try:
            self.wait('Flux reconciled without removing the database fence', reconciled, 180)
            self.database_fenced(site, uid)
        except (Refusal, subprocess.TimeoutExpired, ValueError, KeyError):
            self.operations.append('unsafe fence reconciliation refused; powering off old site')
            self.docker('stop', self.box(site), timeout=150)
            raise
        self.operations.append('verified old database fence after Flux reconciliation')
        return uid

    def database_fenced(self, site, uid):
        cluster = self.resource(site, 'cluster', self.app+'-db')
        self.require(cluster['metadata']['uid'] == uid and
                     cluster['metadata'].get('annotations', {}).get('cnpg.io/fencedInstances') == '["*"]',
                     'old Cluster identity or persistent database fence changed')
        pod = cluster.get('status', {}).get('currentPrimary')
        self.require(pod, 'fenced database instance not observed')
        self.kubectl(site, '-n', self.ns, 'exec', pod, '-c', 'postgres', '--',
                     'sh', '-c', 'pg_ctl status; status=$?; test "$status" -eq 3')
        return True

    def wait_standby_rebuild(self, site, old_uid):
        def observed():
            cluster = self.resource(site, 'cluster', self.app+'-db')
            if cluster['metadata']['uid'] != old_uid:
                if self.sql(site, self.app, 'SELECT pg_is_in_recovery()') == b't':
                    return 'ready', cluster
                return None
            if cluster['metadata'].get('annotations', {}).get('cnpg.io/fencedInstances') != '["*"]':
                return 'unsafe', 'old database fence disappeared during role reconciliation'
            pod = cluster.get('status', {}).get('currentPrimary')
            if pod:
                status = self.kubectl(site, '-n', self.ns, 'exec', pod, '-c', 'postgres', '--',
                                      'sh', '-c', 'pg_ctl status >/dev/null 2>&1; printf "%s" "$?"').strip()
                if status != b'3':
                    return 'unsafe', 'old database process fence is not active'
            return None
        state, result = self.wait('old incarnation remains fenced until replacement is a standby', observed, 1200)
        if state == 'unsafe':
            self.operations.append('unsafe rejoin refused: '+result)
            self.docker('stop', self.box(site), timeout=150)
            raise Refusal(result)
        self.require(result['metadata'].get('annotations', {}).get('cnpg.io/fencedInstances') in (None, '[]'),
                     'replacement inherited the old database fence')
        self.operations.append('verified fenced replacement '+json.dumps(
            {'Site':site, 'OldClusterUID':old_uid, 'NewClusterUID':result['metadata']['uid']}, sort_keys=True))

    def backup(self, site='pub', name=None):
        name = name or self.app
        backup = 'drill-' + uuid.uuid4().hex[:12]
        obj = {'apiVersion':'postgresql.cnpg.io/v1', 'kind':'Backup', 'metadata':{'name':backup,'namespace':self.ns},
               'spec':{'cluster':{'name':name+'-db'},'method':'plugin','pluginConfiguration':{'name':'barman-cloud.cloudnative-pg.io'}}}
        self.kubectl(site, 'apply', '-f', '-', stdin=json.dumps(obj).encode())
        result = self.wait('completed Barman Backup/' + backup,
                           lambda: (b if b.get('status', {}).get('phase', '').lower() == 'completed' else None)
                           if (b := self.resource(site, 'backup', backup)) else None, 1200)
        self.require(result['status']['phase'].lower() == 'completed', 'backup not completed')
        return backup

    @contextlib.contextmanager
    def failed_backup_uploads(self, site):
        """Deny only run-owned base tar uploads; keep metadata and WAL writable."""
        app = self.current(site=site)
        archive = app['ArchiveID']+'-'+site
        generation = (app.get('Archive') or {}).get(site, 1)
        if generation > 1:
            archive += '-g'+str(generation)
        bucket = self.vault['Bucket']
        prior = self.signed_s3('get-bucket-policy', '--bucket', bucket)
        if prior.returncode:
            self.require(b'NoSuchBucketPolicy' in prior.stderr, 'cannot establish prior bucket policy')
            original = None
        else:
            original = json.loads(prior.stdout)['Policy']
        policy = json.loads(original) if original else {'Version':'2012-10-17', 'Statement':[]}
        if isinstance(policy['Statement'], dict):
            policy['Statement'] = [policy['Statement']]
        resource = 'arn:aws:s3:::'+bucket+'/'+self.ns+'/'+self.app+'/'+archive+'/base/*/*.tar*'
        policy['Statement'].append({'Sid':'RecoveryDrill'+self.runid, 'Effect':'Deny',
                                   'Principal':'*', 'Action':'s3:PutObject', 'Resource':resource})
        try:
            changed = self.signed_s3('put-bucket-policy', '--bucket', bucket, '--policy', json.dumps(policy))
            self.require(changed.returncode == 0, 'run-owned base upload denial was refused')
            yield
        finally:
            restored = (self.signed_s3('put-bucket-policy', '--bucket', bucket, '--policy', original)
                        if original is not None else self.signed_s3('delete-bucket-policy', '--bucket', bucket))
            self.require(restored.returncode == 0, 'prior bucket policy was not restored')

    def signed_s3(self, operation, *args, denied=False):
        env = os.environ.copy()
        if self.scenario in ('provider-contract','rotation'):
            if denied:
                env['AWS_ACCESS_KEY_ID'],env['AWS_SECRET_ACCESS_KEY'] = (
                    env['WECOLAB_PROVIDER_DENIED_KEY_ID'],env['WECOLAB_PROVIDER_DENIED_SECRET'])
            else:
                env['AWS_ACCESS_KEY_ID'],env['AWS_SECRET_ACCESS_KEY'] = self.vault['KeyID'],self.vault['Key']
            argv = ['-e','AWS_ACCESS_KEY_ID','-e','AWS_SECRET_ACCESS_KEY','-e','AWS_DEFAULT_REGION=us-east-1']
        else:
            argv = ['-e','AWS_ACCESS_KEY_ID=wecolab','-e','AWS_SECRET_ACCESS_KEY=wecolab-dev-vault','-e','AWS_DEFAULT_REGION=us-east-1']
        command = ['docker','run','--rm','--network',self.network,*argv,
                   'amazon/aws-cli:latest','--endpoint-url',self.vault['Endpoint'],'s3api',operation,*args]
        self.operations.append('signed S3 '+operation+' '+ ' '.join(args))
        return subprocess.run(command,env=env,capture_output=True,timeout=120)
    def metadata(self, name=None, site='pub', *, allow_failed=False):
        name = name or self.app
        app = self.current(name, site=site)
        archive_id = app.get('ArchiveID') or app['Database']
        generation = (app.get('Archive') or {}).get(site, 1)
        archive = archive_id + '-' + site + ('-g' + str(generation) if generation > 1 else '')
        prefix = self.ns + '/' + name + '/' + archive + '/base/'
        out = self.signed_s3('list-objects-v2','--bucket',self.vault['Bucket'],'--prefix',prefix)
        self.require(out.returncode == 0, 'signed metadata listing failed')
        candidates = []
        for obj in json.loads(out.stdout).get('Contents', []):
            key = obj['Key']
            if not key.startswith(prefix):
                continue
            backup_id, separator, suffix = key[len(prefix):].partition('/')
            if backup_id and separator and suffix == 'backup.info':
                candidates.append(key)
        self.require(candidates, 'no Barman backup.info for current archive')
        eligible = []
        for key in candidates:
            result = self.signed_s3('get-object','--bucket',self.vault['Bucket'],'--key',key,'/dev/stdout')
            self.require(result.returncode == 0, 'signed backup.info fetch failed')
            fields = dict(line.split('=',1) for line in result.stdout.decode().splitlines() if '=' in line)
            if (fields.get('status') == 'DONE' and fields.get('systemid') and fields.get('timeline')
                or allow_failed and fields.get('status') in ('STARTED','FAILED')):
                eligible.append({'ArchiveID':archive_id,'BackupID':key.split('/')[-2], 'SystemID':fields.get('systemid',''),
                                 'Timeline':int(fields.get('timeline') or 0), 'ArchiveName':archive, 'MetadataKey':key,
                                 'CompletedAt':fields.get('end_time',''), 'StartedAt':fields.get('begin_time',''),
                                 'Status':fields['status']})
        self.require(eligible, 'no eligible DONE Barman metadata in current archive')
        return max(eligible, key=lambda x: (x['CompletedAt'] or x['StartedAt'], x['BackupID']))

    def restored(self, expected, *, name=None, site='pub', archive_override=None, record=True):
        name = name or self.app
        source = self.resource(site, 'objectstore', name + '-db-vault')
        config = source['spec']['configuration']
        target = 'read-' + uuid.uuid4().hex[:12]
        store = {'apiVersion': source['apiVersion'], 'kind':'ObjectStore', 'metadata':{'name':target+'-vault','namespace':self.ns},
                 'spec':{'configuration':config}}
        archive = archive_override or self.metadata(name, site)
        cluster = {'apiVersion':'postgresql.cnpg.io/v1','kind':'Cluster','metadata':{'name':target,'namespace':self.ns},
                   'spec':{'instances':1,'storage':{'size':'20Gi'},
                           'bootstrap':{'recovery':{'source':'origin','recoveryTarget':{'backupID':archive['BackupID']}}},
                           'externalClusters':[{'name':'origin','plugin':{'name':'barman-cloud.cloudnative-pg.io',
                               'parameters':{'barmanObjectName':target+'-vault','serverName':archive['ArchiveName']}}}]}}
        self.kubectl(site, 'apply', '-f', '-', stdin=json.dumps(store).encode())
        self.kubectl(site, 'apply', '-f', '-', stdin=json.dumps(cluster).encode())
        self.wait('isolated CNPG restore '+target,
                  lambda: c if (c := self.resource(site, 'cluster', target)).get('status',{}).get('phase') == 'Cluster in healthy state' else None, 1200)
        pod = self.resource(site, 'cluster', target).get('status', {}).get('currentPrimary')
        self.require(pod, 'restored CNPG primary not observed')
        actual = self.sql(site, target, 'SELECT n,marker FROM public.wecolab_recovery ORDER BY n', pod=pod, database=name)
        self.versions['postgres'] = self.sql(site, target, 'SHOW server_version', pod=pod, database=name).decode()
        exp_hash, actual_hash = SHA(expected), SHA(actual)
        check = {'Name':'database-readback' if record else 'historical-database-readback','Passed':exp_hash == actual_hash,
                 'ExpectedSHA256':exp_hash,'ActualSHA256':actual_hash}
        self.checks.append(check)
        self.require(exp_hash == actual_hash, 'restored PostgreSQL rows differ')
        if record:
            self.identity = {k: archive[k] for k in ('ArchiveID','BackupID','SystemID','Timeline')}
            self.identity['ExpectedAppSHA'] = self.console('GET',f'/api/apps/{self.ns}/{self.app}/force-preview',site=site)['ExpectedAppSHA']
        self.restore = target
        return target

    def basic_restore(self):
        self.deploy()
        expected = self.sentinel()
        self.backup()
        self.restored(expected)

    def recreate(self):
        self.deploy()
        before = self.sentinel()
        self.backup()
        first = self.metadata()
        self.console('DELETE','/api/apps/'+self.ns+'/'+self.app)
        self.wait('first incarnation removed', lambda: not any(a['Name'] == self.app for a in self.console('GET','/api/state')['apps']))
        self.deploy()
        after = self.sentinel()
        self.require(before != after, 'distinct incarnation sentinel missing')
        self.backup()
        second = self.metadata()
        self.require(first['ArchiveID'] != second['ArchiveID'], 'archive identity collision on same-name recreation')
        self.restored(before, archive_override=first, record=False)
        self.restored(after)

    def partition(self):
        self.deploy()
        expected = self.sentinel()
        self.backup()
        preview = self.console('GET',f'/api/apps/{self.ns}/{self.app}/force-preview')
        self.require(preview['PreviousPrimary'] == 'pub', 'unexpected previous primary')
        self.docker('network','disconnect',self.network,self.box('pub'))
        try:
            self.sql('pub', self.app, "INSERT INTO public.wecolab_recovery VALUES (257,'still-writable')")
            self.require(self.sql('pub',self.app,'SELECT marker FROM public.wecolab_recovery WHERE n=257') == b'still-writable',
                         'partitioned old primary not writable')
            self.console('POST',f'/api/apps/{self.ns}/{self.app}/primary',{'To':'home','Force':True},allowed=(400,409))
        finally:
            self.docker('network','connect','--ip',self.subnet.rsplit('.',1)[0]+'.2',self.network,self.box('pub'))
        old_cluster = self.fence_database('pub')
        self.docker('stop', self.box('pub'), timeout=150)
        self.require(self.docker('inspect','-f','{{.State.Running}}',self.box('pub')).strip() == b'false', 'physical fence not verified')
        # Home's writer is promoted explicitly while pub is powered off.
        self.docker('cp','install.sh',self.box('home')+':/root/install.sh')
        self.docker('exec',self.box('home'),'bash','/root/install.sh','takeover',timeout=300)
        self.force_fenced(preview)
        self.wait('forced target writable',lambda: self.sql('home',self.app,'SELECT pg_is_in_recovery()') == b'f',1200)
        self.require(self.docker('inspect','-f','{{.State.Running}}',self.box('pub')).strip() == b'false', 'old primary reappeared before rebuild')
        self.sql('home', self.app, "INSERT INTO public.wecolab_recovery VALUES (258,'promoted')")
        self.docker('start', self.box('pub'), timeout=150)
        self.wait_standby_rebuild('pub', old_cluster)
        try:
            self.sql('pub',self.app,"INSERT INTO public.wecolab_recovery VALUES (259,'must-refuse')")
        except Refusal:
            pass
        else:
            raise Refusal('rebuilt old primary accepted a write after fencing')
        recovered = self.sql('home',self.app,'SELECT n,marker FROM public.wecolab_recovery ORDER BY n')
        self.require(recovered in (expected+b'\n258|promoted',
                                   expected+b'\n257|still-writable\n258|promoted'),
                     'promoted target changed committed sentinel or surviving rows')
        self.wait('rebuilt standby replays promoted rows',
                  lambda: self.sql('pub',self.app,'SELECT n,marker FROM public.wecolab_recovery ORDER BY n') == recovered, 300)
        self.backup(site='home')
        self.restored(recovered,site='home')

    def replay_lag(self):
        self.deploy()
        self.sentinel()
        self.backup()
        self.wait('replica present on home',lambda: self.sql('home',self.app,'SELECT pg_is_in_recovery()') == b't',1200)
        def observe(phase, since=None):
            point = self.current()['Protection']['RecoveryPoint']
            evidence = {'Phase':phase, 'RecoveryPoint':{
                key:point[key] for key in ('State','Reason','ObservedAt','ExposureUpperBoundSeconds') if key in point}}
            if since is not None:
                evidence['RequiredObservedAt'] = since.isoformat()
            self.operations.append('recovery-point-observation '+json.dumps(evidence,sort_keys=True))
            return point
        def measured(phase, since):
            point = observe(phase, since)
            return point if point['State'] == 'within-objective' and point.get('ObservedAt') and (
                dt.datetime.fromisoformat(point['ObservedAt'].replace('Z','+00:00')) >= since) else None
        baseline_phase = 'measured healthy recovery before replay pause'
        baseline_at = dt.datetime.now(dt.timezone.utc)
        baseline = self.wait(baseline_phase,lambda: measured(baseline_phase,baseline_at),180)
        replica = self.dbpod('home')
        self.sql('home',self.app,'SELECT pg_wal_replay_pause()',pod=replica)
        try:
            for n in range(257,513):
                self.sql('pub',self.app,f"INSERT INTO public.wecolab_recovery VALUES ({n},'lag-{n}')")
            paused_phase = 'fresh measured replay outside the five-minute objective'
            lag = self.wait(paused_phase,
                            lambda: p if (p:=observe(paused_phase))['State'] == 'outside-objective' else None,450)
        finally:
            self.sql('home',self.app,'SELECT pg_wal_replay_resume()',pod=replica)
        resumed_at = dt.datetime.now(dt.timezone.utc)
        replay_target = self.sql('pub',self.app,'SELECT pg_current_wal_lsn()').decode()
        # Archive-only replicas cannot replay a target in an open WAL segment.
        self.sql('pub',self.app,'SELECT pg_switch_wal()')
        self.wait('replica catches up',lambda: self.sql('home',self.app,'SELECT count(*) FROM public.wecolab_recovery') == b'512',1200)
        # Later primary WAL must not move the catch-up target on every probe.
        self.wait('post-resume primary write position replayed',
                  lambda: self.sql('home',self.app,f"SELECT pg_last_wal_replay_lsn() >= '{replay_target}'::pg_lsn") == b't',120)
        resumed_phase = 'measured healthy recovery after replay resumes'
        recovered = self.wait(resumed_phase,lambda: measured(resumed_phase,resumed_at),180)
        self.operations.append('verified recovery-confidence-transitions '+json.dumps(
            {'BeforePause':baseline,'Paused':lag,'AfterResume':recovered},sort_keys=True))
        vault = self.prefix+'-vault'
        try:
            self.docker('stop',vault,timeout=120)
            self.sql('pub',self.app,"INSERT INTO public.wecolab_recovery VALUES (513,'archive-paused')")
            self.wait('archival outage visible',lambda: self.current()['Protection']['Database']['State'] != 'protected',600)
        finally:
            self.docker('start',vault,timeout=120)
        self.wait('archive recovered',lambda: self.current()['Protection']['Database']['State'] == 'protected',600)
        self.wait('archival outage rows replayed',lambda: self.sql('home',self.app,'SELECT count(*) FROM public.wecolab_recovery') == b'513',900)
        previous_timeline=int(self.sql('pub',self.app,'SELECT timeline_id FROM pg_control_checkpoint()'))
        expected = self.sql('pub',self.app,'SELECT n,marker FROM public.wecolab_recovery ORDER BY n')
        self.backup()
        self.restored(expected,record=False)
        preview=self.console('GET',f'/api/apps/{self.ns}/{self.app}/force-preview')
        old_cluster = self.fence_database('pub')
        self.docker('stop',self.box('pub'),timeout=150)
        self.docker('cp','install.sh',self.box('home')+':/root/install.sh')
        self.docker('exec',self.box('home'),'bash','/root/install.sh','takeover',timeout=300)
        self.force_fenced(preview)
        def promoted():
            timeline = int(self.sql('home',self.app,
                           'SELECT timeline_id FROM pg_control_checkpoint() WHERE NOT pg_is_in_recovery()') or b'0')
            return timeline if timeline > previous_timeline else None
        promoted_timeline = self.wait('new timeline becomes writable',promoted,1200)
        self.docker('start',self.box('pub'),timeout=150)
        self.wait_standby_rebuild('pub', old_cluster)
        self.wait('rebuilt standby replays the promoted timeline and original rows',
                  lambda: int(self.sql('pub',self.app,'SELECT timeline_id FROM pg_control_checkpoint()')) == promoted_timeline and
                  self.sql('pub',self.app,'SELECT n,marker FROM public.wecolab_recovery ORDER BY n') == expected, 300)
        automatic = self.metadata(site='home')
        self.require(automatic['Status'] == 'DONE' and automatic['Timeline'] == promoted_timeline,
                     'automatic backup did not cover the promoted timeline before rebuilding')
        self.operations.append('verified automatic post-promotion backup '+json.dumps(automatic, sort_keys=True))
        self.backup(site='home')
        self.restored(expected,site='home')

    def rotation(self):
        self.require(os.getenv('WECOLAB_DISPOSABLE_B2_ACCOUNT') == '1', 'rotation requires explicitly authorized disposable B2 account')
        self.require(os.getenv('WECOLAB_B2_ACCOUNT_KEY_ID') and os.getenv('WECOLAB_B2_ACCOUNT_SECRET'),
                     'B2 management credentials for disposable account missing')
        # The Settings API never returns account credentials; preserve the owner-labelled fabric's
        # pre-existing value before the temporary swap and restore it on every exit path.
        prior = self.resource('pub', 'secret', 'storage', 'wecolab-system')['data']
        old = {key:base64.b64decode(prior[field]).decode() for key,field in (('KeyID','key-id'),('Key','key'))}
        self.ns = 'rot-' + self.runid
        self.console('POST','/api/projects',{'Name':self.ns})
        self.grant(self.ns, 'pub')
        self.grant(self.ns, 'home')
        try:
            self.console('POST','/api/settings/storage',
                         {'KeyID':os.environ['WECOLAB_B2_ACCOUNT_KEY_ID'],'Key':os.environ['WECOLAB_B2_ACCOUNT_SECRET']})
            vault = self.console('POST','/api/storage/vault',{'Project':self.ns})
            self.vault['Bucket'],self.vault['Endpoint'] = vault['bucket'],vault['endpoint']
            self.deploy()
            def current_key(site='pub'):
                data = self.resource(site,'secret',self.app)['data']
                return tuple(base64.b64decode(data[k]).decode() for k in ('b2-key-id','b2-key','key-version'))
            keyid,key,version = current_key()
            self.vault['KeyID'],self.vault['Key'] = keyid,key
            expected = self.sentinel()
            self.backup()
            self.restored(expected,record=False)
            replicas = {site:self.resource(site,'deployment','wecolab-warden','wecolab-system')['spec']['replicas']
                        for site in ('pub','home')}
            try:
                self.kubectl('home','-n','wecolab-system','scale','deployment/wecolab-warden','--replicas=0')
                def rotate(site):
                    return self.console('POST','/api/storage/vault/'+self.ns+'/rotate',{},site=site)
                with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                    jobs = [pool.submit(rotate,site) for site in ('pub','home')]
                    self.kubectl('pub','-n','wecolab-system','scale','deployment/wecolab-warden','--replicas=0')
                    self.kubectl('pub','-n','wecolab-system','scale','deployment/wecolab-warden','--replicas='+str(replicas['pub']))
                    for job in jobs:
                        job.result()
                statuses = [v for v in self.console('GET','/api/storage')['vaults'] if v['Project'] == self.ns]
                self.require(statuses and keyid in statuses[0]['Retiring'], 'live old key retired before delayed site report')
            finally:
                for site in ('pub','home'):
                    self.kubectl(site,'-n','wecolab-system','scale','deployment/wecolab-warden','--replicas='+str(replicas[site]))
            def converged():
                a,b = current_key('pub'),current_key('home')
                return a == b and a[0] != keyid and int(a[2]) >= int(version)+2
            self.wait('both sites converge on last committed key version',converged,1200)
            keyid,key,version = current_key()
            self.vault['KeyID'],self.vault['Key'] = keyid,key
            self.wait('old key retirement after delayed site report',
                      lambda: not next(v for v in self.console('GET','/api/storage')['vaults'] if v['Project'] == self.ns)['Retiring'],1200)
            self.backup()
            self.restored(self.sql('pub',self.app,'SELECT n,marker FROM public.wecolab_recovery ORDER BY n'))
        finally:
            self.console('POST','/api/settings/storage',old)

    def remote_storage(self):
        self.require(os.getenv('WECOLAB_REMOTE_AMD64_KVM') == '1',
                     'remote-storage PVC catalog fixture workspace requires disposable amd64 KVM/Kata box at home')
        arch=self.docker('exec',self.box('home'),'uname','-m').strip()
        self.require(arch == b'x86_64','remote-storage workspace needs actual amd64 home site')
        self.docker('exec',self.box('home'),'test','-c','/dev/kvm')
        self.basic_restore()
        original_ns = self.ns
        self.ns = 'remote-' + self.runid
        self.console('POST','/api/projects',{'Name':self.ns})
        small,large = 'small-'+self.runid,'large-'+self.runid
        state=self.console('GET','/api/state')
        box=next(b['Name'] for s in state['sites'] if s['Name'] == 'home' for b in s['Boxes'] if not b['Laptop'])
        def grant(name,storage):
            self.console('POST','/api/offers',dict(Name=name,Site='home',CPU='2',Memory='8Gi',
                                                  Storage=storage,Boxes=[box],To=[self.ns]))
        name='work-'+self.runid
        body=dict(Name=name,Project=self.ns,Catalog='workspace',Sites=['home'],Primary='home')
        grant(small,'1Gi')
        self.console('POST','/api/deploy',body,allowed=(400,409))
        self.require(not any(a['Namespace'] == self.ns and a['Name'] == name for a in self.console('GET','/api/state')['apps']),
                     'insufficient destination grant still committed App')
        self.console('DELETE','/api/offers/'+small)
        grant(large,'10Gi')
        self.console('POST','/api/deploy',body)
        self.wait('real remote PVC Bound',lambda: next((v for v in json.loads(self.kubectl('home','-n',self.ns,'get','pvc','-o','json'))['items']
                  if v['metadata']['labels'].get('wecolab.io/app') == name and v['status'].get('phase') == 'Bound'),None),1200)
        def workload():
            pods=json.loads(self.kubectl('home','-n',self.ns,'get','pods','-l','app='+name,'-o','json'))['items']
            return next((p['metadata']['name'] for p in pods if p['status'].get('phase') == 'Running'),None)
        pod=self.wait('Kata workspace with remote PVC running',workload,1200)
        content='remote-'+self.runid
        self.kubectl('home','-n',self.ns,'exec',pod,'-c',name,'--','sh','-c','printf '+content+' > /home/ubuntu/recovery-sentinel')
        self.require(self.kubectl('home','-n',self.ns,'exec',pod,'-c',name,'--','cat','/home/ubuntu/recovery-sentinel') == content.encode(),
                     'remote PVC data I/O failed')
        self.console('DELETE','/api/offers/'+large)
        self.console('POST','/api/deploy',dict(body,Name='blocked-'+self.runid),allowed=(403,))
        self.require(not any(a['Namespace'] == self.ns and a['Name'] == 'blocked-'+self.runid for a in self.console('GET','/api/state')['apps']),
                     'revoked destination grant still committed App')
        self.ns=original_ns
    def name_race(self):
        self.basic_restore()
        other = 'race-' + self.runid
        claimant = 'claim-' + self.runid
        self.console('POST','/api/projects',{'Name':other})
        self.grant(other, 'pub')
        body = dict(Name=claimant,Image='nginxinc/nginx-unprivileged:1.27-alpine',Port=8080,Sites=['pub'],Primary='pub',Database=False,Hostname=claimant+'.'+self.zone)
        def claim(ns):
            return self.console('POST','/api/deploy',dict(body,Project=ns),allowed=(200,201,202,409),with_status=True)[0]
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            codes = list(pool.map(claim,(self.ns,other)))
        self.operations.append('public claim HTTP outcomes '+json.dumps(codes))
        self.require(codes.count(409) == 1,'public claim race did not commit exactly one winner and refuse one loser')
        owners = self.wait('committed public claim observed',
                           lambda: [a for a in self.console('GET','/api/state')['apps'] if a['Hostname'] == body['Hostname']])
        self.require(len(owners) == 1, 'cross-project route has !=1 owner')
        loser = other if owners[0]['Namespace'] == self.ns else self.ns
        alt = dict(body,Project=loser,Hostname='other-'+claimant+'.'+self.zone)
        self.console('POST','/api/deploy',alt)
        self.wait('alternate public name observed',
                  lambda: any(a['Namespace'] == loser and a['Hostname'] == alt['Hostname']
                              for a in self.console('GET','/api/state')['apps']))
        self.console('POST','/api/deploy',dict(body,Project=loser),allowed=(409,))
        self.require(any(a['Namespace'] == loser and a['Hostname'] == alt['Hostname']
                         for a in self.console('GET','/api/state')['apps']), 'failed rename stole winning route')
        self.console('DELETE','/api/apps/'+owners[0]['Namespace']+'/'+claimant)
        self.wait('route claim released',lambda: not any(a['Hostname'] == body['Hostname'] for a in self.console('GET','/api/state')['apps']))
        self.console('POST','/api/deploy',dict(body,Project=loser))
        transferred = self.wait('transferred public claim observed',
                                lambda: [a for a in self.console('GET','/api/state')['apps'] if a['Hostname'] == body['Hostname']])
        self.require(len(transferred) == 1 and transferred[0]['Namespace'] == loser,'route did not transfer after release')
        left_ns,right_ns = 'c-'+self.runid,'b-c-'+self.runid
        self.console('POST','/api/projects',{'Name':left_ns})
        self.console('POST','/api/projects',{'Name':right_ns})
        self.grant(left_ns, 'pub')
        self.grant(right_ns, 'pub')
        for ns,name in ((left_ns,'m-b'),(right_ns,'m')):
            self.console('POST','/api/deploy',dict(body,Project=ns,Name=name,Hostname='public-'+ns+'.'+self.zone))
        def mesh(ns,name):
            return self.console('POST',f'/api/apps/{ns}/{name}/mesh',{'Mesh':True},allowed=(200,409),with_status=True)[0]
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            codes = list(pool.map(lambda pair: mesh(*pair),((left_ns,'m-b'),(right_ns,'m'))))
        self.operations.append('mesh claim HTTP outcomes '+json.dumps(codes))
        self.require(codes.count(409) == 1,'mesh claim race did not commit exactly one winner and refuse one loser')
        owners = self.wait('committed mesh claim observed',
                           lambda: [a for a in self.console('GET','/api/state')['apps'] if a['Namespace'] in (left_ns,right_ns) and a['Mesh']])
        self.require(len(owners) == 1,'cross-project mesh collision has !=1 owner')
        self.console('DELETE',f'/api/apps/{owners[0]["Namespace"]}/{owners[0]["Name"]}')
        self.wait('mesh claim released',lambda: not any(a['Namespace'] == owners[0]['Namespace'] and a['Name'] == owners[0]['Name']
                  for a in self.console('GET','/api/state')['apps']))
        next_pair = (right_ns,'m') if owners[0]['Namespace'] == left_ns else (left_ns,'m-b')
        self.require(mesh(*next_pair) == 200,'mesh claim was not released for its next owner')
        transferred = self.wait('transferred mesh claim observed',
                                lambda: [a for a in self.console('GET','/api/state')['apps'] if a['Namespace'] in (left_ns,right_ns) and a['Mesh']])
        self.require(len(transferred) == 1 and (transferred[0]['Namespace'],transferred[0]['Name']) == next_pair,
                     'mesh name did not transfer after committed release')

    def host_mesh_connected(self, guest):
        interfaces = json.loads(self.docker('exec', self.box('home'), 'ip', '-j', '-4', 'addr', 'show', 'dev', 'nebula1'))
        addresses = [a['local'] for link in interfaces for a in link.get('addr_info', []) if a['family'] == 'inet']
        self.require(len(addresses) == 1, 'host probe requires one steward mesh address')
        # Workers may reach their own Kubernetes API, not the manager-only Warden port.
        local = self.docker('exec', self.box('home'), 'curl', '-kfsS', '--max-time', '15',
                            'https://127.0.0.1:6443/cacerts')
        self.require(b'-----BEGIN CERTIFICATE-----' in local, 'steward did not serve its Kubernetes CA')
        remote = self.docker('exec', guest, 'curl', '-kfsS', '--max-time', '15',
                             '--interface', 'nebula1', 'https://'+addresses[0]+':6443/cacerts')
        return remote == local

    def host_failure(self):
        gate = pathlib.Path('cmd/console/testdata/firewall-gate.sh')
        retry = pathlib.Path('hack/dev/host-recovery.sh')
        self.require(gate.is_file() and retry.is_file(), 'disposable host systemd probes missing')
        self.require(os.getenv('WECOLAB_DISPOSABLE_SYSTEMD_GUEST') == '1', 'host-failure requires explicitly disposable systemd guest')
        guest = os.getenv('WECOLAB_HOST_GUEST','')
        self.require(guest and guest not in [self.box(site) for site in ('pub','home','mac')], 'host guest must be separate from fabric')
        label = self.docker('inspect','-f','{{index .Config.Labels "wecolab.dev.owner"}}',guest).strip().decode()
        networks = json.loads(self.docker('inspect','-f','{{json .NetworkSettings.Networks}}',guest))
        self.require(label == self.owner and self.network in networks, 'host guest is foreign or outside disposable network')
        args = [os.getenv('WECOLAB_HOST_K3S_DROPIN',''),os.getenv('WECOLAB_HOST_K3S_AGENT_DROPIN',''),os.getenv('WECOLAB_HOST_FIREWALL_UNIT','')]
        self.require(all(args), 'host-failure requires installer-generated dropins and firewall unit paths inside guest')
        self.run(['docker','exec','-i','-e','WECOLAB_DISPOSABLE_SYSTEMD_GUEST=1',guest,'bash','-s','--',*args],
                 stdin=gate.read_bytes(),timeout=600)
        profile='/etc/apparmor.d/cri-containerd.apparmor.d'
        self.docker('exec',guest,'sh','-c','test -r /sys/kernel/security/apparmor/profiles && test -f '+profile)
        original=self.docker('exec',guest,'sha256sum',profile).split()[0]
        baseline_loaded=self.docker('exec',guest,'sh','-c',
                                    "grep -F 'cri-containerd.apparmor.d (' /sys/kernel/security/apparmor/profiles")
        self.docker('cp','install.sh',guest+':/root/install.sh')
        baseline=self.docker('exec','-i',guest,'bash',stdin=pathlib.Path('hack/dev/snap.sh').read_bytes())
        boxes = {b['Name'] for s in self.console('GET','/api/state')['sites'] if s['Name'] == 'home' for b in s['Boxes']}
        invite=self.console('POST','/api/sites/home/boxes',{'Laptop':False})['invite']
        cmd=['docker','exec','-i','-e','WECOLAB_DEV=1','-e','WECOLAB_CACHE=/cache','-e','WECOLAB_BIN=/src/dist/bin',
             guest,'bash','-c','IFS= read -r invite; exec bash /root/install.sh \"$invite\"']
        self.operations.append('docker exec '+guest+' bash /root/install.sh [invitation redacted]')
        proc=subprocess.Popen(cmd,stdin=subprocess.PIPE,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        proc.stdin.write((invite+'\n').encode())
        proc.stdin.close()
        try:
            self.wait('installer downloaded join bundle before interruption',
                      lambda: (self.docker('exec',guest,'test','-f','/var/lib/wecolab/bundle.json') or True) if proc.poll() is None else None,600)
        finally:
            if proc.poll() is None:
                self.docker('exec',guest,'pkill','-9','-f','^bash /root/install.sh wcl2[.]')
        self.require(proc.wait(timeout=60) != 0, 'installer completed rather than being interrupted')
        self.docker('exec',guest,'test','-f','/var/lib/wecolab/bundle.json')
        self.run(cmd,stdin=(invite+'\n').encode(),timeout=1800,disclose=False)
        joined = self.wait('resumed box registration reaches the Console',
                           lambda: {b['Name'] for s in self.console('GET','/api/state')['sites']
                                    if s['Name'] == 'home' for b in s['Boxes']} - boxes, 180)
        self.require(len(joined) == 1, 'interrupted install did not resume into exactly one box')
        self.wait('guest mesh reaches home before reload injection',
                  lambda: self.host_mesh_connected(guest), 120)
        self.run(['docker','exec','-i','-e','WECOLAB_DISPOSABLE_SYSTEMD_GUEST=1',guest,'bash','-s'],
                 stdin=retry.read_bytes(),timeout=600)
        self.wait('guest mesh reaches home after identical-bundle retry',
                  lambda: self.host_mesh_connected(guest), 120)
        self.docker('exec',guest,'bash','/root/install.sh','uninstall',timeout=600)
        self.require(self.docker('exec',guest,'sha256sum',profile).split()[0] == original,
                     'uninstall did not restore external AppArmor profile')
        self.require(self.docker('exec',guest,'sh','-c',
                     "grep -F 'cri-containerd.apparmor.d (' /sys/kernel/security/apparmor/profiles") == baseline_loaded,
                     'external AppArmor load state differs after uninstall')
        final=self.docker('exec','-i',guest,'bash',stdin=pathlib.Path('hack/dev/snap.sh').read_bytes())
        self.require(SHA(baseline) == SHA(final), 'guest host state differs after uninstall')
        self.console('DELETE','/api/sites/home/boxes/'+joined.pop())
        self.basic_restore()

    def provider_contract(self):
        self.require(os.getenv('WECOLAB_AUTHORIZED_PROVIDER') == '1', 'provider-contract requires authorized separate real provider retention bucket')
        needed = ('WECOLAB_PROVIDER_BUCKET','WECOLAB_PROVIDER_ENDPOINT','WECOLAB_PROVIDER_KEY_ID','WECOLAB_PROVIDER_SECRET',
                  'WECOLAB_PROVIDER_DENIED_KEY_ID','WECOLAB_PROVIDER_DENIED_SECRET')
        self.require(all(os.getenv(v) for v in needed), 'authorized disposable provider bucket and independent denied-delete key are required')
        self.vault = {'Bucket':os.environ['WECOLAB_PROVIDER_BUCKET'],'Endpoint':os.environ['WECOLAB_PROVIDER_ENDPOINT'],
                      'KeyID':os.environ['WECOLAB_PROVIDER_KEY_ID'],'Key':os.environ['WECOLAB_PROVIDER_SECRET']}
        self.require(re.fullmatch(r'[a-z0-9][a-z0-9.-]{2,62}',self.vault['Bucket']), 'invalid disposable provider bucket')
        self.require(self.vault['Endpoint'].startswith('https://'), 'real provider must use HTTPS')
        lock = self.signed_s3('get-object-lock-configuration','--bucket',self.vault['Bucket'])
        versioning = self.signed_s3('get-bucket-versioning','--bucket',self.vault['Bucket'])
        self.require(lock.returncode == 0 and json.loads(lock.stdout).get('ObjectLockConfiguration',{}).get('ObjectLockEnabled') == 'Enabled', 'Object Lock not enabled')
        self.ns = 'prov-' + self.runid
        self.console('POST','/api/projects',{'Name':self.ns})
        self.grant(self.ns, 'pub')
        self.grant(self.ns, 'home')
        self.require(versioning.returncode == 0 and json.loads(versioning.stdout).get('Status') == 'Enabled','bucket versioning disabled')
        self.deploy()
        expected = self.sentinel()
        self.backup()
        observed = self.metadata()
        versions = self.signed_s3('list-object-versions','--bucket',self.vault['Bucket'],'--prefix',observed['MetadataKey'])
        self.require(versions.returncode == 0, 'signed version listing failed')
        matches = [v for v in json.loads(versions.stdout).get('Versions',[]) if v['Key'] == observed['MetadataKey']]
        self.require(matches and matches[0].get('VersionId'), 'backup metadata has no version')
        version = matches[0]['VersionId']
        retained = self.signed_s3('get-object-retention','--bucket',self.vault['Bucket'],'--key',observed['MetadataKey'],'--version-id',version)
        self.require(retained.returncode == 0, 'cannot inspect backup retention')
        retention = json.loads(retained.stdout).get('Retention', {})
        self.require(retention.get('Mode') in ('COMPLIANCE','GOVERNANCE'), 'backup version is not retained')
        raw_deadline = retention.get('RetainUntilDate')
        self.require(isinstance(raw_deadline, str), 'backup retention deadline is missing')
        try:
            deadline = dt.datetime.fromisoformat(raw_deadline.replace('Z', '+00:00'))
        except ValueError:
            raise Refusal('backup retention deadline is invalid') from None
        self.require(deadline.tzinfo is not None and deadline > dt.datetime.now(dt.timezone.utc),
                     'backup retention is expired or has no timezone')
        self.operations.append('observed active backup retention '+json.dumps(
            {'Mode':retention['Mode'],'RetainUntilDate':deadline.isoformat(),'VersionId':version},sort_keys=True))
        attempt = self.signed_s3('delete-object','--bucket',self.vault['Bucket'],'--key',observed['MetadataKey'],'--version-id',version,denied=True)
        self.require(attempt.returncode != 0 and b'AccessDenied' in attempt.stderr,
                     'denied-delete credential did not receive provider AccessDenied')
        self.operations.append('observed denied-delete credential refusal; separate from retention metadata')
        exists = self.signed_s3('head-object','--bucket',self.vault['Bucket'],'--key',observed['MetadataKey'],'--version-id',version)
        self.require(exists.returncode == 0,'retained version vanished after denial')
        self.restored(expected)

    def recipe_rollout(self, recipe, site='pub', *, previous=None):
        workload = self.current()['Workload']
        images = sorted([recipe['image']] + [s['image'] for s in recipe['sidecars']])
        def ready():
            deployment = self.resource(site, 'deployment', workload)
            spec, status = deployment['spec'], deployment.get('status', {})
            replicas = spec.get('replicas', 1)
            current_images = sorted(c['image'] for c in spec['template']['spec']['containers'])
            changed = (previous is None or
                       deployment['metadata']['uid'] != previous['metadata']['uid'] or
                       deployment['metadata']['generation'] > previous['metadata']['generation'])
            if (changed and current_images == images and replicas > 0 and
                status.get('observedGeneration') == deployment['metadata']['generation'] and
                all(status.get(k, 0) == replicas for k in ('replicas', 'updatedReplicas', 'readyReplicas')) and
                status.get('unavailableReplicas', 0) == 0):
                return deployment
            return None
        return self.wait('exact recipe generation and replicas ready', ready, 900)

    def recipe_probe(self, recipe, probe, expected, phase, site='pub'):
        deployment = self.recipe_rollout(recipe, site)
        selector = ','.join(k+'='+v for k, v in deployment['spec']['selector']['matchLabels'].items())
        containers = deployment['spec']['template']['spec']['containers']
        images = {c['name']: c['image'] for c in containers}
        pods = json.loads(self.kubectl(site, '-n', self.ns, 'get', 'pods', '-l', selector, '-o', 'json'))['items']
        pods = [p for p in pods if not p['metadata'].get('deletionTimestamp') and
                {c['name']: c['image'] for c in p['spec']['containers']} == images and
                any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', []))]
        self.require(pods, 'no ready candidate recipe pod')
        node = self.resource(site, 'node', pods[0]['spec']['nodeName'])
        architecture = node.get('status', {}).get('nodeInfo', {}).get('architecture')
        image_ids = {c['name']: c.get('imageID') for c in pods[0].get('status', {}).get('containerStatuses', [])}
        self.require(architecture and set(image_ids) == set(images) and all(image_ids.values()),
                     'recipe runtime architecture or container image identity is missing')
        self.versions['recipe-'+phase+'-architecture'] = architecture
        self.versions['recipe-'+phase+'-images'] = json.dumps(image_ids, sort_keys=True)
        for container in containers:
            row = min([0] + [int(line.split(b'|', 1)[0]) for line in expected.splitlines()]) - 1
            marker = uuid.uuid4().hex
            wanted = f'{row}|{marker}\n'.encode() + expected
            observed = self.kubectl(site, '-n', self.ns, 'exec', '-i', pods[0]['metadata']['name'],
                                    '-c', container['name'], '--', 'sh', '-s', '--', str(row), marker,
                                    stdin=probe).strip()
            actual = self.sql(site, self.app, 'SELECT n,marker FROM public.wecolab_recovery ORDER BY n')
            self.require(observed == wanted and actual == wanted,
                         'recipe container did not write and read the shared application database')
            self.checks.append({'Name':'recipe-'+phase+'-'+container['name']+'-database-readback',
                                'Passed':True,'ExpectedSHA256':SHA(wanted),'ActualSHA256':SHA(observed)})
            expected = wanted
        return expected

    def recipe_lifecycle(self):
        slug = os.getenv('WECOLAB_RECOVERY_CATALOG','')
        previous = self.console('GET','/catalog.json')['entries']
        current = json.loads(pathlib.Path('cmd/console/web/catalog.json').read_text())['entries']
        before = next((e for e in previous if e['slug'] == slug),None)
        after = next((e for e in current if e['slug'] == slug),None)
        self.require(before and after and before.get('database') and after.get('database'), 'pinned candidate must have a PostgreSQL catalog recipe')
        self.require(before['version'] == os.getenv('WECOLAB_RECOVERY_OLD_VERSION') and
                     after['version'] == os.getenv('WECOLAB_RECOVERY_NEW_VERSION') and before['version'] != after['version'],
                     'running and checkout catalogs must expose distinct explicitly pinned recipe versions')
        self.require(before.get('sidecars') and after.get('sidecars'), 'main/sidecar recipe needed')
        self.require(not any(e.get('volumes') or any(s.get('volumes') for s in e['sidecars']) for e in (before,after)),
                     'recipe claims local-only files with no declared independent file restoration path')
        probe_path = pathlib.Path(os.getenv('WECOLAB_RECOVERY_RECIPE_PROBE', ''))
        self.require(probe_path.is_file() and probe_path.stat().st_size > 0,
                     'recipe lifecycle requires an operator-reviewed main/sidecar database probe')
        probe = probe_path.read_bytes()
        self.versions['recipe-probe-sha256'] = SHA(probe)
        self.versions['recipe-old'],self.versions['recipe-new'] = before['version'],after['version']
        self.deploy(catalog=slug)
        original = self.current()['ArchiveID']
        expected = self.sentinel()
        self.sql('pub', self.app, 'GRANT SELECT, INSERT ON public.wecolab_recovery TO "'+self.app+'"')
        expected = self.recipe_probe(before, probe, expected, 'installed')
        self.backup()
        previous_deployment = self.recipe_rollout(before)
        self.run(['bash','hack/dev/fabric.sh','reload'],timeout=1200)
        self.wait('new catalog served',lambda: next((e for e in self.console('GET','/catalog.json')['entries'] if e['slug'] == slug and e['version'] == after['version']),None),300)
        self.console('POST','/api/deploy',dict(Name=self.app,Project=self.ns,Catalog=slug,Sites=['pub','home'],Primary='pub',Database=True,RPO='5m'))
        self.recipe_rollout(after, previous=previous_deployment)
        self.require(self.current()['ArchiveID'] == original,'recipe upgrade changed archive identity')
        self.require(self.sql('pub',self.app,'SELECT n,marker FROM public.wecolab_recovery ORDER BY n') == expected,'recipe upgrade changed database rows')
        expected = self.recipe_probe(after, probe, expected, 'upgraded')
        self.backup()
        def movable():
            app = self.current()
            conditions = {c['type']: c['status'] for c in app.get('Conditions', [])}
            return not app.get('Handover') and all(conditions.get(k) == 'True'
                for k in ('PrimaryHealthy', 'StandbyStaged', 'WithinRPO'))
        self.wait('recipe has current planned-move recovery proof', movable, 900)
        self.console('POST', f'/api/apps/{self.ns}/{self.app}/primary', {'To':'home'})
        def moved():
            app = self.current()
            return app['Active'] == 'home' and app['Ready'] == 'True' and not app.get('Handover')
        self.wait('recipe planned move completed', moved, 1200)
        expected = self.recipe_probe(after, probe, expected, 'moved', site='home')
        self.backup(site='home')
        self.restored(expected, site='home')
        workload, database = self.current()['Workload'], self.current()['Database']
        self.console('DELETE', f'/api/apps/{self.ns}/{self.app}')
        def removed():
            if any(a['Namespace'] == self.ns and a['Name'] == self.app
                   for a in self.console('GET', '/api/state')['apps']):
                return False
            return all(not self.kubectl(site, '-n', self.ns, 'get', 'deployment/'+workload,
                                       'service/'+workload, 'cluster/'+database,
                                       '--ignore-not-found', '-o', 'name').strip()
                       for site in ('pub', 'home'))
        self.wait('recipe uninstall removed both sites workloads and databases', removed, 900)

    def old_writer_mutation_refused(self, giturl, token, site='pub'):
        url = giturl.rstrip('/')+'/api/v1/repos/fabric/fabric/contents/coordination/recovery-'+self.runid+'-downgrade-probe'
        payload = {'branch':'main','message':'owned old-writer refusal probe',
                   'content':base64.b64encode((self.owner+'\n').encode()).decode()}
        config = ('url = '+json.dumps(url)+'\nheader = '+json.dumps('Authorization: token '+token)+
                  '\ndata = '+json.dumps(json.dumps(payload))+'\n')
        code = self.run(['docker','exec','-i',self.box(site),'curl','-sS','--max-time','30',
                         '-o','/dev/null','-w','%{http_code}','-X','POST','-H','Content-Type: application/json',
                         '--config','-'],stdin=config.encode()).strip()
        self.require(code == b'401', 'old-writer mutation did not receive the required credential-revocation refusal')
        self.operations.append('verified old-writer Git mutation refused with HTTP 401')

    def upgrade(self):
        required = ('WECOLAB_UPGRADE_SNAPSHOT','WECOLAB_UPGRADE_RECOVERY_MATERIAL','WECOLAB_UPGRADE_AGE_KEY',
                    'WECOLAB_UPGRADE_WARDEN','WECOLAB_UPGRADE_WARDEN_SHA256','WECOLAB_UPGRADE_VERSION',
                    'WECOLAB_GIT_URL','WECOLAB_UPGRADE_WRITER_TOKEN','WECOLAB_UPGRADE_OLD_WRITER_TOKEN')
        self.require(all(os.getenv(x) for x in required), 'upgrade requires isolated offline snapshot destination, card, verified new Warden digest, pinned version, writer token and age key')
        self.require(os.getenv('WECOLAB_UPGRADE_IMAGES_STAGED') == '1',
                     'stage verified images on both architectures before upgrading')
        for key in ('WECOLAB_UPGRADE_RECOVERY_MATERIAL','WECOLAB_UPGRADE_AGE_KEY','WECOLAB_UPGRADE_WARDEN'):
            self.require(pathlib.Path(os.environ[key]).is_file() and pathlib.Path(os.environ[key]).stat().st_size > 0,
                         'missing offline upgrade input '+key)
        self.require(SHA(pathlib.Path(os.environ['WECOLAB_UPGRADE_WARDEN']).read_bytes()) == os.environ['WECOLAB_UPGRADE_WARDEN_SHA256'],
                     'release Warden binary differs from independently verified digest')
        snapshot=pathlib.Path(os.environ['WECOLAB_UPGRADE_SNAPSHOT'])
        self.require(not snapshot.exists() and snapshot.parent.is_dir() and not snapshot.parent.is_symlink() and
                     not snapshot.parent.resolve().is_relative_to(pathlib.Path.cwd().resolve()),
                     'offline snapshot destination must be unused and outside checkout')
        inventory = self.console('GET','/api/state')
        site_names = sorted(x['Name'] for x in inventory['sites'])
        existing_names = {a['Namespace']+'/'+a['Name'] for a in inventory['apps'] if a['Database']}
        old_proof = os.getenv('WECOLAB_UPGRADE_EXISTING_APPS_PROOF')
        self.require(not existing_names or old_proof and pathlib.Path(old_proof).is_file(),
                     'existing database apps require independent operator restore receipts before maintenance')
        existing = json.loads(pathlib.Path(old_proof).read_text())['apps'] if old_proof else []
        self.require({a['name'] for a in existing} == existing_names,
                     'pre-change receipt must identify every existing database app and no extras')
        self.deploy()
        expected = self.sentinel()
        self.backup()
        self.restored(expected,record=False)
        restored_at = NOW()
        primary = self.metadata()
        before = primary['ArchiveID']
        giturl=os.environ['WECOLAB_GIT_URL'].rstrip('/')
        token=os.environ['WECOLAB_UPGRADE_WRITER_TOKEN']
        self.require(re.fullmatch(r'https?://[A-Za-z0-9.:-]+',giturl) and
                     re.fullmatch(r'[A-Za-z0-9_-]+',token), 'unsupported Forgejo URL or token format')
        api=giturl+'/api/v1/repos/fabric/fabric/archive/main.tar.gz'
        config='url = "'+api+'"\nheader = "Authorization: token '+token+'"\n'
        def old_token_status():
            cfg = 'url = "'+giturl+'/api/v1/user"\nheader = "Authorization: token '+os.environ['WECOLAB_UPGRADE_OLD_WRITER_TOKEN']+'"\n'
            result = subprocess.run(['docker','exec','-i',self.box('pub'),'curl','-sS','-o','/dev/null',
                                     '-w','%{http_code}','--config','-'],input=cfg.encode(),
                                    capture_output=True,timeout=120)
            self.require(result.returncode == 0, 'old controller credential status probe failed')
            return result.stdout.strip()
        self.require(token != os.environ['WECOLAB_UPGRADE_OLD_WRITER_TOKEN'] and old_token_status() == b'200',
                     'old controller credential must differ from replacement and authenticate before fencing')
        self.operations.append('docker exec '+self.box('pub')+' curl -fsS --config - '+api+' [authorization redacted]')
        archive=subprocess.run(['docker','exec','-i',self.box('pub'),'curl','-fsS','--config','-'],
                               input=config.encode(),capture_output=True,timeout=120)
        self.require(archive.returncode == 0 and len(archive.stdout) > 1024,
                     'cannot snapshot the current post-fixture Forgejo main branch')
        fd=os.open(snapshot,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
        with os.fdopen(fd,'wb') as file:
            file.write(archive.stdout)
            file.flush()
            os.fsync(file.fileno())
        inputs = {'snapshot':'WECOLAB_UPGRADE_SNAPSHOT','card':'WECOLAB_UPGRADE_RECOVERY_MATERIAL',
                  'age':'WECOLAB_UPGRADE_AGE_KEY','warden':'WECOLAB_UPGRADE_WARDEN'}
        for name,key in inputs.items():
            self.docker('cp',os.environ[key],self.box('pub')+':/root/recovery-'+self.runid+'-'+name)
        binary = '/root/recovery-'+self.runid+'-warden'
        files = {name:'/root/recovery-'+self.runid+'-'+name for name in inputs}
        self.docker('exec',self.box('pub'),'chmod','700',binary)
        self.versions['upgrade-warden-sha256'] = SHA(pathlib.Path(os.environ['WECOLAB_UPGRADE_WARDEN']).read_bytes())
        self.versions['upgrade-target'] = os.environ['WECOLAB_UPGRADE_VERSION']
        def stage(name,*args,refusal=None):
            argv=['docker','exec','-e','WECOLAB_GIT_URL','-e','WECOLAB_GIT_TOKEN',
                  '-e','KUBECONFIG=/etc/rancher/k3s/k3s.yaml',self.box('pub'),binary,
                  'upgrade','--stage',name,*args]
            env=os.environ.copy()
            env['WECOLAB_GIT_TOKEN']=os.environ['WECOLAB_UPGRADE_WRITER_TOKEN']
            self.operations.append(' '.join(argv).replace(binary,'[verified release warden]'))
            result=subprocess.run(argv,env=env,capture_output=True,timeout=600)
            if result.returncode:
                self.operations.append('upgrade '+name+' exit='+str(result.returncode)+' stderr-sha256='+SHA(result.stderr))
            if refusal:
                self.require(result.returncode != 0 and refusal.encode() in result.stderr,
                             'upgrade '+name+' did not produce the expected schema-report refusal')
            else:
                self.require(result.returncode == 0,'upgrade '+name+' failed; inspect restricted offline upgrade receipt')
        stage('review')
        deployments = {(site,deploy):self.resource(site,'deployment',deploy,'wecolab-system')['spec'].get('replicas',1)
                       for site in ('pub','home') for deploy in ('wecolab-console','wecolab-warden')}
        phase = 'old'
        try:
            for site,deploy in deployments:
                self.kubectl(site,'-n','wecolab-system','scale','deployment/'+deploy,'--replicas=0')
            self.wait('old writer/Console pods stopped',
                      lambda: all(self.resource(site,'deployment',deploy,'wecolab-system')['status'].get('readyReplicas',0) == 0
                                  for site,deploy in deployments),180)
            wal = self.signed_s3('list-objects-v2','--bucket',self.vault['Bucket'],
                                 '--prefix',self.ns+'/'+self.app+'/'+primary['ArchiveName']+'/wals/')
            self.require(wal.returncode == 0,'cannot inspect signed latest WAL for upgrade proof')
            stamps = [x['LastModified'] for x in json.loads(wal.stdout).get('Contents',[]) if x.get('LastModified')]
            self.require(stamps,'no archived WAL for upgrade proof')
            receipt_path = pathlib.Path(os.getenv('WECOLAB_UPGRADE_REVOCATION_RECEIPT',''))
            self.require(str(receipt_path) not in ('','.') and receipt_path.is_absolute() and
                         not receipt_path.parent.resolve().is_relative_to(pathlib.Path.cwd().resolve()),
                         'operator must supply external absolute WECOLAB_UPGRADE_REVOCATION_RECEIPT path')
            self.require(not receipt_path.exists(), 'old token revocation receipt predates writer fence')
            self.wait('operator old Git token revocation receipt',
                      lambda: receipt_path.is_file() and not receipt_path.is_symlink() and receipt_path.read_text().strip() == self.owner,600)
            phase = 'fenced'
            self.require(old_token_status() == b'401',
                         'old controller credential still authenticates after purported revocation')
            receipt = {'snapshotSHA256':SHA(snapshot.read_bytes()),
                       'recoverySHA256':SHA(pathlib.Path(os.environ['WECOLAB_UPGRADE_RECOVERY_MATERIAL']).read_bytes()),
                       'writerFencedAt':NOW(),'writerFenceMethod':'old Console and writer Warden pods stopped; operator separately revoked old Git credential',
                       'sites':site_names,
                       'apps':existing+[{'name':self.ns+'/'+self.app,'archive':primary['ArchiveName'],
                                         'backupID':primary['BackupID'],'systemID':primary['SystemID'],
                                         'backupCompletedAt':dt.datetime.fromisoformat(primary['CompletedAt']).astimezone(dt.timezone.utc).isoformat().replace('+00:00','Z'),
                                         'walObservedAt':max(stamps),'restoredAt':restored_at,
                                         'restoreReadbackSHA256':SHA(expected)}]}
            proof = self.directory/'upgrade-proof.json'
            proof.write_text(json.dumps(receipt,sort_keys=True)+'\n')
            proof.chmod(0o600)
            self.docker('cp',str(proof),self.box('pub')+':/root/recovery-'+self.runid+'-proof')
            files['proof'] = '/root/recovery-'+self.runid+'-proof'
            phase = 'begin'
            stage('begin','--snapshot',files['snapshot'],'--recovery-material',files['card'],
                  '--backup-proof',files['proof'],'--local-site','pub')
            stage('schemas','--version',os.environ['WECOLAB_UPGRADE_VERSION'])
            self.docker('restart',self.box('pub'),timeout=180)
            self.wait('writer manager recovered from interruption',
                      lambda: self.docker('inspect','-f','{{.State.Running}}',self.box('pub')).strip() == b'true',180)
            stage('migrate','--age-key',files['age'])
            stage('migrate','--age-key',files['age'])
            stage('platform','--version',os.environ['WECOLAB_UPGRADE_VERSION'])
            phase = 'platform'
            for site,deploy in deployments:
                self.kubectl(site,'-n','wecolab-system','scale','deployment/'+deploy,'--replicas='+str(deployments[site,deploy]))
            self.wait('new schema-aware writer reports',lambda: self.console('GET','/api/settings').get('isWriter'),600)
            stage('complete','--version','deliberately-mismatched-schema',
                  refusal='has no fresh schema-aware report for deliberately-mismatched-schema')
            self.operations.append('verified schema-report mismatch refusal; not an old-binary downgrade attempt')
            stage('complete','--version',os.environ['WECOLAB_UPGRADE_VERSION'])
            phase = 'complete'
            self.old_writer_mutation_refused(giturl, os.environ['WECOLAB_UPGRADE_OLD_WRITER_TOKEN'])
        finally:
            if phase == 'old':
                try:
                    old_usable = old_token_status() == b'200'
                except (Refusal, subprocess.TimeoutExpired):
                    old_usable = False
                if old_usable:
                    for site,deploy in deployments:
                        self.kubectl(site,'-n','wecolab-system','scale','deployment/'+deploy,'--replicas='+str(deployments[site,deploy]))
                else:
                    self.operations.append('old credential unavailable: controllers remain fenced for operator recovery')
            elif phase == 'platform':
                for site,deploy in deployments:
                    self.kubectl(site,'-n','wecolab-system','scale','deployment/'+deploy,'--replicas='+str(deployments[site,deploy]))
            elif phase != 'complete':
                self.operations.append('upgrade fenced: old writers remain stopped; offline snapshot and receipt retained for operator recovery')
        self.require(self.current()['ArchiveID'] == before,'upgrade changed archive identity')
        self.require(self.sql('pub',self.app,'SELECT n,marker FROM public.wecolab_recovery ORDER BY n') == expected,
                     'upgrade lost pre-change committed rows')
        self.backup()
        self.restored(expected)

    def failed_backup(self):
        self.deploy()
        expected = self.sentinel()
        self.backup()
        prior = self.metadata()
        backup = 'fail-' + uuid.uuid4().hex[:12]
        obj = {'apiVersion':'postgresql.cnpg.io/v1','kind':'Backup','metadata':{'name':backup,'namespace':self.ns},
               'spec':{'cluster':{'name':self.app+'-db'},'method':'plugin',
                       'pluginConfiguration':{'name':'barman-cloud.cloudnative-pg.io'}}}
        with self.failed_backup_uploads('pub'):
            self.kubectl('pub','apply','-f','-',stdin=json.dumps(obj).encode())
            self.require(self.resource('pub','backup',backup)['spec']['cluster']['name'] == self.app+'-db',
                         'backup name did not target run-owned Cluster')
            self.wait('denied base upload fails the CNPG backup',
                      lambda: self.resource('pub','backup',backup).get('status',{}).get('phase','').lower() == 'failed',180)
            observed = self.wait('denied base upload publishes FAILED Barman metadata',
                                 lambda: m if (m:=self.metadata(allow_failed=True))['Status'] == 'FAILED' and
                                 m['BackupID'] != prior['BackupID'] else None,180)
            failure_observed_at = dt.datetime.now(dt.timezone.utc)
            # An automatic backup may finish after the initial baseline snapshot.
            # Compare against the newest DONE metadata after the denied attempt.
            eligible = self.metadata()
            eligible_time = dt.datetime.fromisoformat(eligible['CompletedAt'].replace('Z','+00:00'))
            def selected():
                app = self.current()
                evidence = app['Protection']['Database']
                fresh = next((c for c in app['Conditions'] if c['type'] == 'VaultFresh'), None)
                return (evidence, fresh) if fresh and evidence.get('BackupCompletedAt') and evidence.get('ObservedAt') and (
                    dt.datetime.fromisoformat(evidence['ObservedAt'].replace('Z','+00:00')) >= failure_observed_at) else None
            # Vault scans run five minutes after completion; allow the next scan and its projection.
            evidence, freshness = self.wait('product-selected recovery proof after failed backup',selected,600)
            selected_time = dt.datetime.fromisoformat(evidence['BackupCompletedAt'].replace('Z','+00:00'))
            self.operations.append('observed failed-backup-product-eligibility '+json.dumps(
                {'SelectedCompletedAt':evidence['BackupCompletedAt'],'BackupID':prior['BackupID'],
                 'ExpectedBackupID':eligible['BackupID'],'ExpectedCompletedAt':eligible['CompletedAt'],
                 'DatabaseEvidence':evidence,'VaultFresh':freshness,
                 'FailedBackupID':observed['BackupID'],'FailedMetadataObservedAt':failure_observed_at.isoformat(),
                 'ProtectionObservedAt':evidence['ObservedAt']},sort_keys=True))
            self.require(selected_time == eligible_time and evidence['State'] == 'protected' and freshness['status'] == 'True',
                         'product did not select the latest completed recovery proof after failed backup')
        self.backup()
        retry = self.metadata()
        self.require(retry['BackupID'] not in (prior['BackupID'], observed['BackupID']) and retry['Status'] == 'DONE',
                     'completed retry was not selected after failed backup')
        self.restored(expected,record=False)
        # A forced move creates a new primary archive on home. Unlike the previous
        # generation on pub it has no eligible base backup yet; pub remains physically
        # fenced until its first attempted home backup has failed.
        self.wait('home is a standby before failed-only archive drill',
                  lambda: self.sql('home',self.app,'SELECT pg_is_in_recovery()') == b't')
        old_cluster = self.resource('pub','cluster',self.app+'-db')['metadata']['uid']
        def pvc_ids():
            items = json.loads(self.kubectl('pub','-n',self.ns,'get','pvc','-l',
                            'cnpg.io/cluster='+self.app+'-db','-o','json'))['items']
            return {item['metadata']['name']:item['metadata']['uid'] for item in items}
        old_pvcs = pvc_ids()
        self.require(old_pvcs, 'no old primary PVC identity to protect')
        preview = self.console('GET',f'/api/apps/{self.ns}/{self.app}/force-preview')
        self.require(preview['PreviousPrimary'] == 'pub','failed-only drill expected pub as old primary')
        pub_started = False
        pub_stop_attempted = False
        try:
            with self.failed_backup_uploads('home'):
                self.require(self.fence_database('pub') == old_cluster, 'old Cluster changed before fencing')
                pub_stop_attempted = True
                self.docker('stop',self.box('pub'),timeout=150)
                self.require(self.docker('inspect','-f','{{.State.Running}}',self.box('pub')).strip() == b'false',
                             'old primary not physically fenced')
                self.docker('cp','install.sh',self.box('home')+':/root/install.sh')
                self.docker('exec',self.box('home'),'bash','/root/install.sh','takeover',timeout=300)
                self.force_fenced(preview)
                self.wait('home promoted and writable without a completed home backup',
                          lambda: self.sql('home',self.app,'SELECT pg_is_in_recovery()') == b'f',1200)
                def home_failed():
                    meta = self.metadata(site='home',allow_failed=True)
                    self.require(meta['Status'] != 'DONE','new primary archive already has a completed backup')
                    backups = json.loads(self.kubectl('home','-n',self.ns,'get','backups','-o','json'))['items']
                    failed = [b for b in backups if b['spec']['cluster']['name'] == self.app+'-db' and
                              b.get('status',{}).get('phase','').lower() == 'failed']
                    return failed if meta['Status'] == 'FAILED' and failed else None
                self.wait('failed base upload on first home archive',home_failed,600)
                failed_home = self.metadata(site='home',allow_failed=True)
                self.require(failed_home['Status'] == 'FAILED' and
                             failed_home['ArchiveName'].endswith('-home'),
                             'new primary archive did not retain failed Barman metadata')
                self.require(self.current(site='home')['Protection']['Database']['State'] != 'protected',
                             'failed-only primary archive incorrectly qualified as protected')
                self.sql('home',self.app,"INSERT INTO public.wecolab_recovery VALUES (257,'new-primary-survivor')")
                surviving = self.sql('home',self.app,'SELECT n,marker FROM public.wecolab_recovery ORDER BY n')
                self.require(surviving == expected+b'\n257|new-primary-survivor',
                             'new primary did not retain original committed rows')
                self.docker('start',self.box('pub'),timeout=150)
                pub_started = True
                self.wait('old database remains fenced after container restart',
                          lambda: self.database_fenced('pub', old_cluster), 180)
                def refused():
                    cr = self.resource('pub','apps.wecolab.io',self.app)
                    conditions = cr.get('status',{}).get('conditions',[])
                    return next((c for c in conditions if c.get('type') == 'Promotion' and
                                 c.get('reason') == 'RebuildWaiting' and
                                 'backup' in c.get('message','').lower()),None)
                self.wait('old database rebuild refused without completed new-archive backup',refused,600)
                self.require(self.resource('pub','cluster',self.app+'-db')['metadata']['uid'] == old_cluster and
                             pvc_ids() == old_pvcs, 'failed-only backup permitted deletion of old Cluster or PVC')
                for _ in range(3):
                    self.reconcile_database_fence('pub', old_cluster)
                self.operations.append('verified old process fence through three post-role Flux reconciliations')
                self.operations.append('verified failed-only-archive-destructive-rebuild-refused '+json.dumps(
                    {'OldClusterUID':old_cluster,'OldPVCUIDs':old_pvcs,
                     'FailedBackupID':failed_home['BackupID']},sort_keys=True))
            self.backup(site='home')
            good_home = self.metadata(site='home')
            self.require(good_home['BackupID'] != failed_home['BackupID'] and good_home['Status'] == 'DONE',
                         'new primary archive still lacks independently completed backup')
            self.wait_standby_rebuild('pub', old_cluster)
            self.require(self.sql('pub',self.app,'SELECT pg_is_in_recovery()') == b't',
                         'rebuilt old primary did not remain a standby')
            self.restored(surviving,site='home')
        finally:
            if pub_stop_attempted and not pub_started:
                self.operations.append('recovery interrupted before confirmed rejoin; retain old database fence')

    def save(self, outcome):
        if not self.directory.exists():
            return
        doc = {'Version':1,'Scenario':self.scenario,'Outcome':outcome,'StartedAt':self.started,'CompletedAt':NOW(),
               'Namespace':self.ns,'App':self.app,'ArchiveID':self.identity.get('ArchiveID',''),
               'SystemID':self.identity.get('SystemID',''),'Timeline':self.identity.get('Timeline',0),
               'BackupID':self.identity.get('BackupID',''),'ExpectedAppSHA':self.identity.get('ExpectedAppSHA',''),'Scope':self.scope,
               'ComponentVersions':self.versions,'Operations':self.operations,'Checks':self.checks}
        path = self.directory / 'result.json'
        path.write_text(json.dumps(doc,indent=2,sort_keys=True)+'\n')
        print(path, file=sys.stderr)

    def execute(self):
        self.directory.mkdir(mode=0o700,parents=True, exist_ok=False)
        try:
            self.preflight()
            getattr(self,self.scenario.replace('-','_'))()
            self.require(any(c['Name'] == 'database-readback' and c['Passed'] for c in self.checks), 'completed scenario lacks real restored database readback')
        except BaseException:
            self.save('failed')
            raise
        self.save('passed')

if __name__ == '__main__':
    if len(sys.argv) == 2 and sys.argv[1] in ('--help','-h'):
        print('usage: hack/dev/recovery.sh <'+ '|'.join(SCENARIOS) +'>')
        print('Requires WECOLAB_DEV_PREFIX, WECOLAB_DEV_OWNER, WECOLAB_DEV_SUBNET and labelled disposable pub/home/mac/vault.')
        print('WECOLAB_RECOVERY_ARTIFACTS sets a per-run sanitized result directory (default: system temporary directory).')
        print('Provider, amd64/KVM, systemd guest and upgrade fixtures require explicit scenario-specific credentials and inputs.')
        print('Recipe inputs: WECOLAB_RECOVERY_CATALOG, WECOLAB_RECOVERY_OLD_VERSION, WECOLAB_RECOVERY_NEW_VERSION, WECOLAB_RECOVERY_RECIPE_PROBE.')
        print('The reviewed recipe probe must use the container database driver, commit $1/$2 to wecolab_recovery, and print ordered n|marker rows; see docs/development.md.')
        sys.exit(0)
    if len(sys.argv) != 2:
        print('usage: hack/dev/recovery.sh <'+ '|'.join(SCENARIOS) +'>',file=sys.stderr)
        sys.exit(2)
    try:
        r = Runner(sys.argv[1])
        r.execute()
    except (Refusal, subprocess.TimeoutExpired, OSError, ValueError, KeyError) as e:
        print('recovery prerequisite or assertion failed: '+str(e),file=sys.stderr)
        sys.exit(1)
