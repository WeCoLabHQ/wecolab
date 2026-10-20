"""Build identities must describe the complete source checkout, not CI checkout depth."""
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

GENERATOR = pathlib.Path(__file__).with_name('build-info.py')


class BuildInfoTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name) / 'source'
        self.root.mkdir()
        self.git('init', '-q')
        (self.root / 'VERSION').write_text('0.1.0\n')
        (self.root / '.gitignore').write_text('output/\n')
        self.commit()

    def git(self, *args, root=None):
        env = os.environ.copy()
        # Do not inherit the surrounding worktree's Git routing or signing policy.
        for key in ('GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR'):
            env.pop(key, None)
        return subprocess.check_output(
            ['git', '-C', str(root or self.root), '-c', 'user.name=Build Test',
             '-c', 'user.email=build-test@example.invalid', '-c', 'commit.gpgsign=false',
             *args], text=True, stderr=subprocess.PIPE, env=env).strip()

    def commit(self):
        self.git('add', '.')
        self.git('commit', '-qm', 'fixture')

    def generate(self, root=None):
        return subprocess.run(
            [sys.executable, str(GENERATOR), '--root', str(root or self.root)],
            capture_output=True, text=True)

    def info(self):
        result = self.generate()
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_commits_advance_build_without_changing_product_version(self):
        first = self.info()
        self.assertEqual(first['productVersion'], '0.1.0')
        self.assertEqual(first['buildNumber'], 1)
        self.assertEqual(first['sourceRevision'], self.git('rev-parse', 'HEAD'))
        self.assertFalse(first['dirty'])
        (self.root / 'source.txt').write_text('new source\n')
        self.commit()
        second = self.info()
        self.assertEqual(second['productVersion'], first['productVersion'])
        self.assertEqual(second['buildNumber'], 2)
        self.assertNotEqual(second['sourceRevision'], first['sourceRevision'])
        self.assertFalse(second['dirty'])

    def test_untracked_staged_and_unstaged_sources_are_dirty(self):
        original = self.info()
        source = self.root / 'source.txt'
        source.write_text('untracked\n')
        self.assertTrue(self.info()['dirty'])
        self.git('add', 'source.txt')
        staged = self.info()
        self.assertTrue(staged['dirty'])
        self.assertEqual(staged['sourceRevision'], original['sourceRevision'])
        self.assertEqual(staged['buildNumber'], original['buildNumber'])
        self.commit()
        self.assertFalse(self.info()['dirty'])
        source.write_text('modified\n')
        self.assertTrue(self.info()['dirty'])

    def test_ignored_build_outputs_do_not_change_identity(self):
        original = self.info()
        output = self.root / 'output'
        output.mkdir()
        (output / 'binary').write_bytes(b'build output')
        self.assertEqual(self.info(), original)

    def test_shallow_checkout_refuses_misleading_build_number(self):
        (self.root / 'source.txt').write_text('second commit\n')
        self.commit()
        clone = pathlib.Path(self.temp.name) / 'shallow'
        self.git('clone', '-q', '--depth=1', self.root.as_uri(), str(clone))
        result = self.generate(clone)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('shallow', result.stderr.lower())
        self.assertEqual(result.stdout, '')

    def test_product_version_requires_three_numeric_components(self):
        for invalid in ('', 'dev', 'v0.1.0', '01.1.0', '0.1', '0.1.0\ninjected', '0.1.0-rc.1'):
            with self.subTest(version=invalid):
                (self.root / 'VERSION').write_text(invalid)
                result = self.generate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('VERSION', result.stderr)
                self.assertEqual(result.stdout, '')


if __name__ == '__main__':
    unittest.main()
