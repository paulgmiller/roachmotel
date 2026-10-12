FROM python:3.13.7-slim-bookworm
RUN pip install --no-cache-dir 'psycopg[binary]==3.2.10'
WORKDIR /app
COPY app.py /app/app.py
USER 10001:10001
CMD ["python", "-u", "/app/app.py"]
