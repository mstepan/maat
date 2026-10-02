#!/usr/bin/env python3
"""Run integration scenarios in a fresh Compose project and stop it afterward."""
import json
import shutil
import sys
import tempfile

import integration
from transition_crashes import CrashLab


def main():
    lab = None

    def create_lab(timeout):
        nonlocal lab
        directory = tempfile.mkdtemp(prefix="maat-integration-", dir=integration.ROOT / "bin")
        binary = shutil.copy2(integration.ROOT / "bin" / "maat-linux", directory)
        lab = CrashLab(timeout, directory, binary, project_prefix="maat-integration")
        lab.event("integration project prepared", project=lab.project, directory=directory)
        lab.create()
        return lab

    status = 1
    try:
        status = integration.main(lab_factory=create_lab, description=__doc__)
    except (integration.Failure, OSError, KeyboardInterrupt) as error:
        print(json.dumps({"integration_error": str(error)}), file=sys.stderr)
    finally:
        if lab is not None:
            try:
                lab.stop()
            except (integration.Failure, OSError) as error:
                status = 1
                print(json.dumps({"shutdown_error": str(error)}), file=sys.stderr)
    return status


if __name__ == "__main__":
    sys.exit(main())
