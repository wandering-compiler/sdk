# The conformance suite's Python business tier: the SDK from this checkout,
# the fixed NoteService, the suite's descriptor set. Build context: the
# repository root (`make conformance-py`).
FROM python:3.13-slim
COPY py /src/py
RUN pip install --no-cache-dir /src/py
COPY conformance/testdata /app/testdata
COPY conformance/python /app/python
WORKDIR /app/python
ENTRYPOINT ["python", "-m", "w17_business", "serve", "--manifest", "/app/python/w17-business.json", "--project-root", "/app/python"]
