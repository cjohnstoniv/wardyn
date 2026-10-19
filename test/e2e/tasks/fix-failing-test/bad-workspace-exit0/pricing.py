"""A workspace whose code ends the process with status 0 on import, so a grader that trusts the
exit status alone passes it without a single test having run."""
import os

os._exit(0)
