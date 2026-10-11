# The Pipecat benchmark bot.
FROM python:3.13-slim-bookworm
ENV PYTHONUNBUFFERED=1
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY bot.py .
EXPOSE 7860
CMD ["python", "bot.py", "--host", "0.0.0.0", "--port", "7860"]