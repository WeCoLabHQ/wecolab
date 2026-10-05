#!/usr/bin/env python3
"""Import the HomelabOS catalog (Ansible roles with docker-compose templates) into
WeCoLab catalog entries: one JSON document the Console embeds.

Usage: python3 hack/catalog-import.py /path/to/HomelabOS > cmd/console/web/catalog.json
Needs jinja2 and pyyaml (python3 -m venv /tmp/venv && /tmp/venv/bin/pip install jinja2 pyyaml).

The translation is mechanical and honest: a role becomes one workload (its main
container), a CloudNativePG database when the compose has Postgres, sidecars in
the same pod for other support containers, and a volume per data path. Anything
that needs the host (devices, privileged, host network, the docker socket) is
listed but marked unsupported, with the reason. WeCoLab's own entries, in
hack/catalog-own.json, are put first as they are.
"""
import os, sys, re, json, subprocess, collections, datetime
import yaml, jinja2
from jinja2 import ChainableUndefined

ROOT = sys.argv[1].rstrip('/')
ROLES = f'{ROOT}/roles'
SUPPORT = {'postgres', 'postgresql', 'redis', 'valkey', 'mariadb', 'mysql', 'mongo', 'memcached', 'rabbitmq',
           'elasticsearch', 'meilisearch', 'influxdb', 'clickhouse', 'minio', 'nginx', 'guacd', 'opensearch', 'typesense', 'nats', 'gotenberg', 'tika', 'chromium', 'browserless'}
PG_CUSTOM = ('pgvector', 'vectorchord', 'postgis', 'timescale', 'pgautoupgrade', 'paradedb')
BASELINE_CAPS = {'AUDIT_WRITE', 'CHOWN', 'DAC_OVERRIDE', 'FOWNER', 'FSETID', 'KILL', 'MKNOD', 'NET_BIND_SERVICE', 'SETFCAP', 'SETGID', 'SETPCAP', 'SETUID', 'SYS_CHROOT'}
CATEGORY = {'misc-other': 'misc', 'note-taking-and-editors': 'note-taking-and-editor', 'e-books': 'ebooks', 'social': 'social-networking', 'miscfederated-identityauthentication': 'federated-identity-authentication'}


class U(ChainableUndefined):
    def __str__(self): return '${' + (self._undefined_name or 'x') + '}'
    def __iter__(self): return iter(())
    def __bool__(self): return False
    def __json__(self): return str(self)


class Vars(dict):
    def __getitem__(self, k):
        return dict.__getitem__(self, k) if k in self else U(name=k)
    def __getattr__(self, k):
        return self[k] if k in self else U(name=k)


def jsonable(x):
    if isinstance(x, jinja2.Undefined): return str(x)
    if isinstance(x, dict): return {k: jsonable(v) for k, v in x.items()}
    if isinstance(x, (list, tuple)): return [jsonable(v) for v in x]
    return x


def render(role, svc):
    v = Vars(svc); v.setdefault('enable', True)

    def lookup(kind, *a, **kw):
        if kind == 'vars':
            return v if (a and a[0] == role) else Vars()
        if kind == 'password':
            m = re.search(r'passwords/' + re.escape(role) + r'_([a-z0-9_]+?)(\s|$)', a[0]) if a else None
            return '${secret:' + (m.group(1) if m else 'secret') + '}'
        if kind == 'env':
            return '${env:' + (a[0] if a else 'x') + '}'
        return U(name='lookup')
    os.makedirs('/tmp/wecolab-stubs', exist_ok=True)
    open('/tmp/wecolab-stubs/labels.j2', 'w').write('')
    env = jinja2.Environment(loader=jinja2.FileSystemLoader([f'{ROLES}/{role}/templates', '/tmp/wecolab-stubs']), undefined=U)
    ident = lambda s, *a, **k: str(s)
    for f in ('b64encode', 'hash', 'password_hash', 'quote', 'ansible.builtin.b64encode', 'urlencode', 'lower', 'upper'):
        env.filters[f] = ident
    env.filters['lower'] = lambda s: str(s).lower(); env.filters['upper'] = lambda s: str(s).upper()
    env.filters['to_json'] = lambda x, *a, **k: json.dumps(jsonable(x)); env.filters['to_nice_json'] = env.filters['to_json']
    env.filters['from_json'] = json.loads; env.filters['bool'] = lambda x: bool(x)
    env.filters['regex_replace'] = lambda s, a, b='': re.sub(a, b, str(s))
    env.filters['regex_search'] = lambda s, a, *k: (re.search(a, str(s)).group(0) if re.search(a, str(s)) else '')
    env.filters['basename'] = os.path.basename; env.filters['dirname'] = os.path.dirname
    env.tests['defined'] = lambda x: not isinstance(x, jinja2.Undefined); env.tests['undefined'] = lambda x: isinstance(x, jinja2.Undefined)
    ctx = dict(service_item=role, volumes_root='${volumes_root}', storage_dir='${storage_dir}', common_timezone='${TZ}', domain='${domain}',
               service_domain='${hostname}', default_username='${admin_user}', default_password='${secret:admin_password}', admin_email='${admin_email}',
               uid_output={'stdout': '1000'}, gid_output={'stdout': '1000'}, smtp=Vars(), aws=Vars(), traefik=Vars({'dns_challenge_provider': False}),
               https_only=True, auth=False, authelia=Vars({'enable': False}), custom_domain=False, enable_tor=False, enable_sslip=False, lookup=lookup,
               hostvars=Vars(), inventory_hostname='host', ansible_host='host', homelab_ip='${ip}', tor_domain='x', gitlab_registry_name='registry', svc=Vars())
    ctx[role] = v
    # additional_configs.yml holds the role's defaults (db_version, redis_version, ...) as a template over its own settings.
    ap = f'{ROLES}/{role}/additional_configs.yml'
    if os.path.exists(ap):
        try:
            extra = yaml.safe_load(env.from_string(open(ap).read()).render(**ctx)) or {}
            for k, val in (extra.items() if isinstance(extra, dict) else []):
                if isinstance(val, (str, int, float)) and '${' not in str(val):
                    v.setdefault(k, val)
        except Exception:
            pass
    text = env.get_template(f'docker-compose.{role}.yml.j2').render(**ctx)
    return yaml.safe_load(text)


def envmap(e):
    out = {}
    if isinstance(e, dict):
        for k, v in e.items(): out[str(k)] = '' if v is None else str(v)
    elif isinstance(e, list):
        for item in e:
            k, _, v = str(item).partition('=')
            out[k] = v
    return out


# What a compose key asks of the host, as the catalog says it (a project's pod gets none of it).
HOST_NEEDS = {'privileged': 'privileged mode, which controls the whole box', 'devices': "the host's devices",
              'network_mode': "the host's network", 'pid': "to see the host's processes", 'ipc': "the host's shared memory"}


def volumes(spec, role):
    """host:container[:mode] -> (name, mountPath, readOnly) or a flag."""
    out, flags = [], []
    for v in spec.get('volumes') or []:
        if isinstance(v, dict):
            src, dst = str(v.get('source', '')), str(v.get('target', ''))
        else:
            parts = str(v).split(':'); src, dst = parts[0], (parts[1] if len(parts) > 1 else '')
        ro = str(v).endswith(':ro')
        if not dst: continue
        if 'docker.sock' in src: flags.append('needs the Docker socket, which controls the whole box'); continue
        if src.startswith('/dev/'): flags.append("needs one of the host's devices"); continue
        if src in ('/etc/localtime', '/etc/timezone', '/etc/hosts'): continue
        if src.startswith('${volumes_root}/') or src.startswith('${storage_dir}/') or src.startswith('/') or src.startswith('${'):
            name = re.sub(r'[^a-z0-9]+', '-', src.replace('${volumes_root}/' + role, '').replace('${storage_dir}', 'storage').replace('${volumes_root}', 'data').lower()).strip('-') or 'data'
        else:
            name = re.sub(r'[^a-z0-9]+', '-', src.lower()).strip('-') or 'data'  # named volume
        out.append({'name': name[:40], 'mountPath': dst, 'readOnly': ro})
    return out, flags


def entry(role, svc, doc, validation):
    services = {k: v for k, v in (doc.get('services') or {}).items() if isinstance(v, dict)}
    if not services: return None
    if role in services:
        main = role
    else:
        cands = [n for n, s in services.items() if str(s.get('image', '')).split(':')[0].split('/')[-1] not in SUPPORT]
        main = cands[0] if cands else next(iter(services))
    m = services[main]
    e = {'slug': role, 'title': svc.get('full_service_name') or role, 'description': (svc.get('description') or '').strip(),
         'category': CATEGORY.get(svc.get('category', 'misc'), svc.get('category', 'misc')), 'url': svc.get('project_url', ''),
         'version': str(svc.get('version', '')), 'image': str(m.get('image', '')), 'port': int(svc.get('port') or 80),
         'oidc': bool(svc.get('instance_aware_oidc_config')), 'env': [], 'volumes': [], 'sidecars': [], 'database': None,
         'unsupported': [], 'attention': [], 'secrets': [], 'state': 'none'}
    if m.get('command') is not None: e['command'] = m['command'] if isinstance(m['command'], list) else str(m['command'])
    if m.get('entrypoint') is not None: e['entrypoint'] = m['entrypoint'] if isinstance(m['entrypoint'], list) else str(m['entrypoint'])
    if m.get('user'): e['user'] = str(m['user'])
    names = set(services)
    pg = None
    for n, s in services.items():
        if n == main: continue
        img = str(s.get('image', ''))
        base = img.split(':')[0].split('/')[-1]
        if ('postgres' in base or base in ('pgvecto-rs', 'postgis', 'timescaledb')) and pg is None:
            pg = (n, s)
            if any(x in img for x in PG_CUSTOM) or base not in ('postgres', 'postgresql'):
                e['unsupported'].append(f'database needs a custom Postgres image ({img}); CloudNativePG runs plain Postgres')
            continue
        sc = {'name': re.sub(r'[^a-z0-9]+', '-', n.lower()).strip('-')[:40], 'image': img, 'env': [], 'volumes': []}
        if s.get('command') is not None: sc['command'] = s['command'] if isinstance(s['command'], list) else str(s['command'])
        vols, fl = volumes(s, role); sc['volumes'] = vols; e['unsupported'] += fl
        for k, v in envmap(s.get('environment')).items():
            sc['env'].append(resolve(k, v, names, None, e))
        e['sidecars'].append(sc)
        if vols: e['state'] = 'volume'
    if pg:
        pn, ps = pg
        penv = envmap(ps.get('environment'))
        e['database'] = {'user': penv.get('POSTGRES_USER', 'app'), 'name': penv.get('POSTGRES_DB', penv.get('POSTGRES_USER', 'app')), 'password': penv.get('POSTGRES_PASSWORD', '')}
        e['state'] = 'database'
    for k, v in envmap(m.get('environment')).items():
        e['env'].append(resolve(k, v, names, (pg[0], e['database']) if pg else None, e))
    vols, fl = volumes(m, role); e['volumes'] = vols; e['unsupported'] += fl
    if vols and e['state'] == 'none': e['state'] = 'volume'
    if vols and e['state'] == 'database': e['state'] = 'database+volume'
    # host access the sites forbid
    for key, what in HOST_NEEDS.items():
        if m.get(key): e['unsupported'].append('needs ' + what)
    caps = set(str(c).upper().removeprefix('CAP_') for c in (m.get('cap_add') or []))
    if caps - BASELINE_CAPS: e['unsupported'].append('needs extra Linux capabilities: ' + ', '.join(sorted(caps - BASELINE_CAPS)))
    if m.get('sysctls'): e['attention'].append('sets sysctls')
    if m.get('security_opt'): e['attention'].append('security_opt: ' + ', '.join(map(str, m['security_opt'])))
    if m.get('env_file'): e['attention'].append('reads an env_file the import cannot see')
    if m.get('ports'): e['ports'] = [str(p) for p in m['ports']]; e['attention'].append('publishes extra ports the Door does not route')
    unresolved = sorted({x for x in re.findall(r'\$\{([a-z_.]+)\}', json.dumps(e)) if not x.startswith('secret:') and x not in ('hostname', 'domain', 'TZ', 'admin_user', 'admin_email', 'ip')})
    if unresolved: e['attention'].append('unresolved settings: ' + ', '.join(unresolved[:8]))
    e['unsupported'] = sorted(set(e['unsupported'])); e['attention'] = sorted(set(e['attention']))
    e['secrets'] = sorted({x for x in re.findall(r'\$\{secret:([a-z0-9_]+)\}', json.dumps(e))})
    v = validation.get(role) or {}
    e['validated'] = v.get('disposition', 'unknown')
    e['limitations'] = [l.get('feature') if isinstance(l, dict) else str(l) for l in (v.get('limitations') or [])][:4]
    return e


def resolve(k, v, names, pg, e):
    """One env var: a literal, a generated secret, a placeholder the Console fills, a database field, or a sidecar host."""
    item = {'name': k}
    if pg:
        pn, db = pg
        if re.search(r'@' + re.escape(pn) + r'(:\d+)?/', v) or re.search(r'(host|server)=' + re.escape(pn) + r'\b', v, re.I):
            item['db'] = 'uri'; return item
        if v == pn: item['db'] = 'host'; return item
        if v == db['password'] and v: item['db'] = 'password'; return item
        if v == db['user'] and re.search('user', k, re.I): item['db'] = 'user'; return item
        if v == db['name'] and re.search('db|database|name', k, re.I): item['db'] = 'dbname'; return item
        if v == '5432' and re.search('port', k, re.I): item['db'] = 'port'; return item
    for n in names:
        if n in v and n != v and re.search(r'(^|[@/:\s])' + re.escape(n) + r'([:/\s]|$)', v):
            v = re.sub(r'(^|[@/:\s])' + re.escape(n) + r'([:/\s]|$)', lambda mm: mm.group(1) + 'localhost' + mm.group(2), v)
        elif v == n:
            v = 'localhost'
    v = v.replace(e['slug'] + '.${domain}', '${hostname}')
    item['value'] = v
    return item


def main():
    validation = {}
    vp = f'{ROOT}/docs/development/service-validation-results.json'
    if os.path.exists(vp):
        validation = json.load(open(vp)).get('roles', {})
    entries, failed = [], []
    for role in sorted(os.listdir(ROLES)):
        sp, tp = f'{ROLES}/{role}/service.yml', f'{ROLES}/{role}/templates/docker-compose.{role}.yml.j2'
        if not (os.path.exists(sp) and os.path.exists(tp)): continue
        try:
            svc = yaml.safe_load(open(sp)) or {}
            doc = render(role, svc)
            ent = entry(role, svc, doc, validation)
            if ent: entries.append(ent)
        except Exception as ex:
            failed.append({'slug': role, 'error': f'{type(ex).__name__}: {str(ex)[:120]}'})
    # WeCoLab's own entries (a workspace, decision 26) come first, as written.
    entries = json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)), 'catalog-own.json'))) + entries
    commit = subprocess.run(['git', '-C', ROOT, 'rev-parse', 'HEAD'], capture_output=True, text=True).stdout.strip()
    out = {'generated': datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'),
           'source': {'repo': 'https://gitlab.com/vincehark/HomelabOS', 'branch': 'feat/service-batch', 'commit': commit},
           'entries': entries, 'failed': failed}
    json.dump(out, sys.stdout, indent=1, sort_keys=True)
    c = collections.Counter()
    for e in entries:
        c['total'] += 1; c['deployable'] += not e['unsupported']; c['database'] += bool(e['database']); c['sidecars'] += bool(e['sidecars']); c['volumes'] += bool(e['volumes']); c['attention'] += bool(e['attention'])
    print(f"entries {c['total']} deployable {c['deployable']} database {c['database']} sidecars {c['sidecars']} volumes {c['volumes']} attention {c['attention']} failed {len(failed)}", file=sys.stderr)


main()
