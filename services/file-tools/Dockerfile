FROM python:3.14-slim
RUN apt-get update && apt-get install -y --no-install-recommends libcairo2 libgomp1 fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY requirements.txt ./
RUN python -m venv /opt/venv && /opt/venv/bin/pip install --no-cache-dir -r requirements.txt
ENV PATH=/opt/venv/bin:$PATH U2NET_HOME=/opt/models OMP_NUM_THREADS=2 OPENBLAS_NUM_THREADS=2 NUMBA_CACHE_DIR=/tmp/numba PYTHONDONTWRITEBYTECODE=1
RUN python -c 'from rembg import new_session; new_session("birefnet-general-lite", providers=["CPUExecutionProvider"])'
RUN chmod -R a+rX /opt/models
COPY worker.py server.py test_worker.py test_server.py ./
RUN python -m unittest discover -s /app -p "test_*.py" -v && rm test_worker.py test_server.py
USER 1000:1000
EXPOSE 8080
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8080", "--no-access-log", "--limit-concurrency", "4"]
