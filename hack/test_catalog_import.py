"""Exercise the importer through its real role/template input format."""
import datetime
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

IMPORTER = pathlib.Path(__file__).with_name('catalog-import.py')


def imported(services, overlay=None):
    with tempfile.TemporaryDirectory() as root:
        root = pathlib.Path(root)
        role = root / 'roles' / 'sample'
        (role / 'templates').mkdir(parents=True)
        (role / 'service.yml').write_text('full_service_name: Sample\nport: 8080\n')
        (role / 'templates' / 'docker-compose.sample.yml.j2').write_text(services)
        env = os.environ.copy()
        overlay_path = root / 'certifications.json'
        overlay_path.write_text(json.dumps({'sample': overlay} if overlay is not None else {}))
        env['WECOLAB_CATALOG_CERTIFICATIONS'] = str(overlay_path)
        result = subprocess.run([sys.executable, str(IMPORTER), str(root)], capture_output=True, text=True, env=env)
        if result.returncode:
            raise ValueError(result.stderr)
        return next(e for e in json.loads(result.stdout)['entries'] if e['slug'] == 'sample')


class ImportTests(unittest.TestCase):
    def test_database_identity_precedes_sidecar_localization(self):
        entry = imported('''services:
  sample:
    image: example/web:1
    environment:
      DB_HOST: pg-main
      DB_USER: appuser
      DB_NAME: appdata
      DB_PASSWORD: unique_pw
      DB_PORT: '5432'
      DB_URL: postgresql://appuser:unique_pw@pg-main:5432/appdata
  worker:
    image: example/worker:1
    environment:
      DB_HOST: pg-main
      DB_USER: appuser
      DB_NAME: appdata
      DB_PASSWORD: unique_pw
      DB_PORT: '5432'
      DB_URL: postgresql://appuser:unique_pw@pg-main:5432/appdata
      CACHE_HOST: cache
      OTHER_HOST: other-pg-main
  cache:
    image: redis:7
  pg-main:
    image: postgres:16
    environment:
      POSTGRES_USER: appuser
      POSTGRES_DB: appdata
      POSTGRES_PASSWORD: unique_pw
''')
        expected = {'DB_HOST': 'host', 'DB_USER': 'user', 'DB_NAME': 'dbname',
                    'DB_PASSWORD': 'password', 'DB_PORT': 'port', 'DB_URL': 'uri'}
        worker = next(s for s in entry['sidecars'] if s['name'] == 'worker')
        for items in (entry['env'], worker['env']):
            self.assertEqual({v['name']: v['db'] for v in items if 'db' in v}, expected)
        self.assertEqual({v['name']: v['value'] for v in worker['env'] if 'value' in v},
                         {'CACHE_HOST': 'localhost', 'OTHER_HOST': 'other-pg-main'})

    def test_no_database_sidecar_hosts_stay_local(self):
        entry = imported('''services:
  sample:
    image: example/web:1
  worker:
    image: example/worker:1
    environment:
      CACHE_HOST: cache
      OTHER_HOST: other-cache
  cache:
    image: redis:7
''')
        self.assertEqual(entry['database'], None)
        self.assertEqual({v['name']: v['value'] for v in entry['sidecars'][0]['env']},
                         {'CACHE_HOST': 'localhost', 'OTHER_HOST': 'other-cache'})

    def test_uri_with_extra_semantics_requires_attention(self):
        entry = imported('''services:
  sample:
    image: example/web:1
    environment:
      DB_URL: postgresql://different:pw@pg-main:5432/other?sslmode=require
  pg-main:
    image: postgres:16
    environment:
      POSTGRES_USER: appuser
      POSTGRES_DB: appdata
      POSTGRES_PASSWORD: unique_pw
''')
        self.assertNotEqual(entry['env'][0].get('db'), 'uri')
        self.assertTrue(entry['unsupported'] or entry['attention'])

    def test_main_directory_mount_is_a_persistent_volume(self):
        entry = imported('''services:
  sample:
    image: example/web:1
    volumes:
      - ${volumes_root}/sample/data:/var/lib/sample:rw
''')
        self.assertEqual(entry['volumes'], [{'name': 'data', 'mountPath': '/var/lib/sample', 'readOnly': False}])
        self.assertEqual(entry['state'], 'volume')
        self.assertFalse(entry['unsupported'])

    def test_main_volume_with_database_keeps_uri_and_certification(self):
        today = datetime.date.today().isoformat()
        record = dict(status='passed', testedImage='example/web:1', architecture='amd64',
                      testedAt=today, evidence='https://example.org/drill',
                      databaseRestore='passed', volumeRestore='passed',
                      maintainer='WeCoLab maintainers', securityNotices=[],
                      updateCadence={'interval': 'quarterly', 'lastReviewed': today})
        entry = imported('''services:
  sample:
    image: example/web:1
    environment:
      DATABASE_URL: postgresql://appuser:unique_pw@pg-main:5432/appdata
    volumes:
      - ./data:/var/lib/sample
  pg-main:
    image: postgres:16
    environment:
      POSTGRES_USER: appuser
      POSTGRES_DB: appdata
      POSTGRES_PASSWORD: unique_pw
''', record)
        self.assertEqual(entry['volumes'], [{'name': 'data', 'mountPath': '/var/lib/sample', 'readOnly': False}])
        self.assertEqual(entry['state'], 'database+volume')
        self.assertEqual(entry['env'], [{'name': 'DATABASE_URL', 'db': 'uri'}])
        self.assertEqual(entry['wecolab']['status'], 'passed')
        self.assertFalse(entry['unsupported'])

    def test_file_mount_is_not_imported_as_directory_claim(self):
        entry = imported('''services:
  sample:
    image: example/web:1
    volumes:
      - ./settings.yml:/etc/sample/settings.yml:ro
''')
        self.assertEqual(entry['volumes'], [])
        self.assertEqual(entry['state'], 'none')
        self.assertTrue(any('file mount /etc/sample/settings.yml' in reason for reason in entry['unsupported']))

    def test_no_overlay_never_certifies(self):
        entry = imported('''services:
  sample:
    image: example/web:1
''')
        self.assertNotEqual(entry.get('wecolab', {}).get('status'), 'passed')

    def test_certification_needs_exact_image_architecture_and_restore(self):
        today = datetime.date.today().isoformat()
        record = dict(status='passed', testedImage='example/web:1', architecture='amd64',
                      testedAt=today, evidence='https://example.org/drill',
                      databaseRestore='passed', volumeRestore='not-applicable',
                      maintainer='WeCoLab maintainers', securityNotices=[],
                      updateCadence={'interval': 'quarterly', 'lastReviewed': today})
        compose = '''services:
  sample:
    image: example/web:1
  pg-main:
    image: postgres:16
'''
        self.assertEqual(imported(compose, record)['wecolab']['status'], 'passed')
        for change in ({'testedImage': 'example/web:2'}, {'architecture': ''},
                       {'databaseRestore': 'not-tested'}, {'testedAt': None}):
            self.assertEqual(imported(compose, record | change)['wecolab']['status'], 'not-tested')
        old = (datetime.date.today() - datetime.timedelta(days=93)).isoformat()
        self.assertEqual(imported(compose, record | {'updateCadence': {'interval': 'quarterly', 'lastReviewed': old}})['wecolab']['status'], 'not-tested')
        future = (datetime.date.today() + datetime.timedelta(days=1)).isoformat()
        self.assertEqual(imported(compose, record | {'testedAt': future})['wecolab']['status'], 'not-tested')
        self.assertEqual(imported(compose, record | {'status': 'failed'})['wecolab']['status'], 'failed')
        with self.assertRaises(ValueError):
            imported(compose, record | {'status': 'unknown'})
