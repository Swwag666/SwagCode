/*! Приёмочные тесты картинок (rev69).

Вынесены отдельным модулем, чтобы проверять картинки целиком в одном месте: и
то, что Anthropic получает их блоками в последнем сообщении пользователя, и то,
что они НЕ утекают в тело OpenAI-совместимого запроса.
!*/

use crate::anthropic::anthropic_body;
use crate::types::{ChatMessage, ChatRequest, ImageAttachment};

fn png() -> ImageAttachment {
    ImageAttachment {
        media_type: "image/png".to_string(),
        data_base64: "AAAA".to_string(),
    }
}

#[test]
fn images_go_to_the_last_user_message_only() {
    /* Привязка по индексу, а не к первому user-сообщению: иначе картинка уехала
    бы в старый ход, и модель увидела бы её не там, где спрашивают. */
    let req = ChatRequest::new(
        "m",
        vec![
            ChatMessage::user("старое"),
            ChatMessage::assistant("ok"),
            ChatMessage::user("что на фото?"),
        ],
    )
    .with_images(vec![png()]);

    let body = anthropic_body(&req);
    let msgs = body["messages"].as_array().expect("messages");
    assert_eq!(
        msgs[0]["content"].as_array().expect("блоки").len(),
        1,
        "старый ход без картинок"
    );

    let last = msgs.last().expect("последнее")["content"]
        .as_array()
        .expect("блоки")
        .clone();
    assert_eq!(last.len(), 2, "картинка и текст");
    assert_eq!(last[0]["type"], serde_json::json!("image"));
    assert_eq!(last[0]["source"]["type"], serde_json::json!("base64"));
    assert_eq!(
        last[0]["source"]["media_type"],
        serde_json::json!("image/png")
    );
    assert_eq!(last[0]["source"]["data"], serde_json::json!("AAAA"));
    assert_eq!(
        last[1]["type"],
        serde_json::json!("text"),
        "текст идёт после картинки"
    );
}

#[test]
fn request_without_images_is_unchanged() {
    let req = ChatRequest::new("m", vec![ChatMessage::user("привет")]);
    let body = anthropic_body(&req);
    let last = body["messages"][0]["content"].as_array().expect("блоки");
    assert_eq!(last.len(), 1);
    assert_eq!(last[0]["type"], serde_json::json!("text"));
}

#[test]
fn image_only_message_is_valid() {
    /* Пустой текст с картинкой - законный запрос: блоков нет, кроме картинки, и
    сообщение не должно исчезнуть из тела. */
    let req = ChatRequest::new("m", vec![ChatMessage::user("")]).with_images(vec![png()]);
    let body = anthropic_body(&req);
    let last = body["messages"][0]["content"].as_array().expect("блоки");
    assert_eq!(last.len(), 1);
    assert_eq!(last[0]["type"], serde_json::json!("image"));
}

#[test]
fn images_do_not_leak_into_the_openai_body() {
    /* У OpenAI-совместимых провайдеров верхнеуровневого `images` нет, и строгий
    ответил бы 400. Поле живёт только для сборщика тела по протоколу. */
    let req = ChatRequest::new("m", vec![ChatMessage::user("привет")]).with_images(vec![png()]);
    let v = serde_json::to_value(&req).expect("сериализуется");
    assert!(
        v.get("images").is_none(),
        "в тело OpenAI картинки не уходят: {v}"
    );
    assert_eq!(v["messages"][0]["content"], serde_json::json!("привет"));
}
