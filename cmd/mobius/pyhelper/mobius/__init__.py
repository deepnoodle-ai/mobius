"""Mobius helpers for workflow script steps.

The managed worker puts this package on PYTHONPATH for every job it runs.
Use it from a ``python:`` step::

    from mobius import actions
    rows = actions.run("mobius.table.find_rows", {"table_name": "leads"})
"""
