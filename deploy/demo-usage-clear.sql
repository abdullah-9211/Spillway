DELETE FROM usage_daily WHERE api_key_id IN (SELECT id FROM api_keys WHERE name = 'demo-data');
DELETE FROM usage WHERE api_key_id IN (SELECT id FROM api_keys WHERE name = 'demo-data');
DELETE FROM api_keys WHERE name = 'demo-data';
