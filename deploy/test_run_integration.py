import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import integration
import run_integration
from transition_crashes import CrashLab


class IntegrationIsolationTest(unittest.TestCase):
    def test_projects_have_separate_state_and_fencing_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            labs = [CrashLab(1, Path(directory) / str(i), __file__, "maat-integration") for i in range(2)]
            self.assertNotEqual(labs[0].project, labs[1].project)
            for lab in labs:
                self.assertTrue(lab.project.startswith("maat-integration-"))
                self.assertNotEqual(lab.cluster, "maat_dev")
                self.assertEqual(lab.network, lab.project + "_default")
                document = json.loads((lab.directory / "compose.json").read_text())
                for node, service in document["services"].items():
                    config = json.loads((lab.directory / (node + ".json")).read_text())
                    self.assertEqual(service["labels"]["maat.cluster"], config["cluster_id"])
                    self.assertEqual(config["cluster_id"], lab.cluster)
                    self.assertEqual(service["ports"], ["127.0.0.1::8000"])
                    for volume in service["volumes"][:2]:
                        self.assertEqual(document["volumes"][volume.split(":")[0]], {})
                for secret in document["secrets"].values():
                    self.assertEqual(Path(secret["file"]).parent, lab.directory)

                # Inherited scenario operations must use the isolated project too.
                with patch.object(lab, "command", return_value="") as command, \
                        patch.object(lab, "identity"), patch.object(lab, "event"), \
                        patch.object(lab, "inspect", return_value={"NetworkSettings": {"Networks": {}}}):
                    node = integration.NODES[0]
                    lab.ids[node] = "test-container"
                    integration.Lab.sql(lab, node, "SELECT 1")
                    self.assertEqual(command.call_args.args[0][:len(lab.compose)], lab.compose)
                    lab.restore_network(node)
                    self.assertEqual(command.call_args.args[0],
                                     ["docker", "network", "connect", "--alias", node, lab.network, "test-container"])

    def test_shutdown_on_success_test_failure_and_partial_startup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            (root / "bin" / "maat-linux").write_bytes(b"test binary")
            for test_status, start_error, stop_error in ((0, False, False), (1, False, False),
                                                        (0, True, False), (0, False, True)):
                with self.subTest(test_status=test_status, start_error=start_error, stop_error=stop_error), \
                        patch.object(integration, "ROOT", root), patch.object(run_integration, "CrashLab") as factory, \
                        patch.object(integration, "main") as runner, contextlib.redirect_stderr(io.StringIO()):
                    lab = factory.return_value
                    lab.project = "maat-integration-test"
                    if start_error:
                        lab.create.side_effect = integration.Failure("startup failed")
                    if stop_error:
                        lab.stop.side_effect = integration.Failure("shutdown failed")
                    def run(lab_factory, description):
                        lab_factory(1)
                        return test_status
                    runner.side_effect = run
                    self.assertEqual(run_integration.main(), int(bool(test_status or start_error or stop_error)))
                    lab.stop.assert_called_once_with()


if __name__ == "__main__":
    unittest.main()
