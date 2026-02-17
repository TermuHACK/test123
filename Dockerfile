# Используем легковесный образ Python на базе Alpine
FROM python:3.10-alpine

# Устанавливаем рабочую директорию
WORKDIR /app

# Устанавливаем зависимости для сборки (если понадобятся для некоторых либ)
# и часовой пояс (необязательно, но полезно для логов)
RUN apk add --no-cache tzdata

# Копируем только файл зависимостей, чтобы кэшировать слои Docker
COPY requirements.txt .

# Устанавливаем библиотеки
RUN pip install --no-cache-dir -r --break-system-packages requirements.txt

# Копируем остальной код сервера
COPY . .

# Создаем папку для загрузок, если её нет
RUN mkdir -p uploads

# Выставляем порт (Render прокинет свой через переменную $PORT)
EXPOSE 5000

# Запуск приложения
CMD ["python", "server.py"]
