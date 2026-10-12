.PHONY: render test policy-test sql-test
render:
	python3 -m roachmotel.render
test:
	python3 -m unittest discover -s tests -v
policy-test: render
	PYTHONPATH=. python3 tests/check_kyverno.py
sql-test:
	PYTHONPATH=. python3 tests/check_sql.py
