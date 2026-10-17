FROM python:3.13-slim
RUN pip install --no-cache-dir 'psycopg[binary]==3.2.10'
WORKDIR /app
COPY deploy/fluent-bit/writer.py /app/writer.py
USER 65532:65532
ENTRYPOINT ["python3", "-B", "/app/writer.py"]
