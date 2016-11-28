require 'test_helper'
require 'oj'
require 'database_cleaner'

DatabaseCleaner.strategy = :deletion

class WebsocketTest < ActionDispatch::IntegrationTest
  self.use_transactional_fixtures = false

  setup do
    DatabaseCleaner.start
  end

  teardown do
    DatabaseCleaner.clean
  end

  def self.startup
    s = TCPServer.new('0.0.0.0', 0)
    @@port = s.addr[1]
    s.close
    @@pidfile = "tmp/pids/passenger.#{@@port}.pid"
    DatabaseCleaner.start
    Dir.chdir(Rails.root) do |apidir|
      # Only passenger seems to be able to run the websockets server
      # successfully.
      _system('passenger', 'start', '-d',
              "-p#{@@port}",
              "--log-file", "/dev/stderr",
              "--pid-file", @@pidfile)
      timeout = Time.now.tv_sec + 10
      begin
        sleep 0.2
        begin
          server_pid = IO.read(@@pidfile).to_i
          good_pid = (server_pid > 0) and (Process.kill(0, pid) rescue false)
        rescue Errno::ENOENT
          good_pid = false
        end
      end while (not good_pid) and (Time.now.tv_sec < timeout)
      if not good_pid
        raise RuntimeError, "could not find API server Rails pid"
      end
      STDERR.puts "Started websocket server on port #{@@port} with pid #{server_pid}"
    end
  end

  def self.shutdown
    Dir.chdir(Rails.root) do
      _system('passenger', 'stop', "-p#{@@port}",
              "--pid-file", @@pidfile)
    end
    # DatabaseCleaner leaves the database empty. Prefer to leave it full.
    dc = DatabaseController.new
    dc.define_singleton_method :render do |*args| end
    dc.reset
  end

  def self._system(*cmd)
    Bundler.with_clean_env do
      env = {
        'ARVADOS_WEBSOCKETS' => 'ws-only',
        'RAILS_ENV' => 'test',
      }
      if not system(env, *cmd)
        raise RuntimeError, "Command exited #{$?}: #{cmd.inspect}"
      end
    end
  end

  def ws_helper(token: nil, timeout: 8)
    opened = false
    close_status = nil
    too_long = false

    EM.run do
      if token
        ws = Faye::WebSocket::Client.new("ws://localhost:#{@@port}/websocket?api_token=#{api_client_authorizations(token).api_token}")
      else
        ws = Faye::WebSocket::Client.new("ws://localhost:#{@@port}/websocket")
      end

      ws.on :open do |event|
        opened = true
        if timeout
          EM::Timer.new(timeout) do
            too_long = true if close_status.nil?
            EM.stop_event_loop
          end
        end
      end

      ws.on :error do |event|
        STDERR.puts "websocket client error: #{event.inspect}"
      end

      ws.on :close do |event|
        close_status = [:close, event.code, event.reason]
        EM.stop_event_loop
      end

      yield ws
    end

    assert opened, "Should have opened web socket"
    assert (not too_long), "Test took too long"
    assert_equal 1000, close_status[1], "Connection closed unexpectedly (check log for errors)"
  end

  test "connect with no token" do
    status = nil

    ws_helper do |ws|
      ws.on :message do |event|
        d = Oj.strict_load event.data
        status = d["status"]
        ws.close
      end
    end

    assert_equal 401, status
  end

  test "connect, subscribe and get response" do
    status = nil

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe'}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        status = d["status"]
        ws.close
      end
    end

    assert_equal 200, status
  end

  def subscribe_test
    state = 1
    spec = nil
    ev_uuid = nil

    authorize_with :active

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe'}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          spec = Specimen.create
          state = 2
        when 2
          ev_uuid = d["object_uuid"]
          ws.close
        end
      end

    end

    assert_not_nil spec
    assert_equal spec.uuid, ev_uuid
  end

  test "connect, subscribe, get event" do
    subscribe_test()
  end

  test "connect, subscribe, get two events" do
    state = 1
    spec = nil
    human = nil
    spec_ev_uuid = nil
    human_ev_uuid = nil

    authorize_with :active

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe'}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          spec = Specimen.create
          human = Human.create
          state = 2
        when 2
          spec_ev_uuid = d["object_uuid"]
          state = 3
        when 3
          human_ev_uuid = d["object_uuid"]
          state = 4
          ws.close
        when 4
          assert false, "Should not get any more events"
        end
      end

    end

    assert_not_nil spec
    assert_not_nil human
    assert_equal spec.uuid, spec_ev_uuid
    assert_equal human.uuid, human_ev_uuid
  end

  test "connect, subscribe, filter events" do
    state = 1
    human = nil
    human_ev_uuid = nil

    authorize_with :active

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe', filters: [['object_uuid', 'is_a', 'arvados#human']]}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          Specimen.create
          human = Human.create
          state = 2
        when 2
          human_ev_uuid = d["object_uuid"]
          state = 3
          ws.close
        when 3
          assert false, "Should not get any more events"
        end
      end

    end

    assert_not_nil human
    assert_equal human.uuid, human_ev_uuid
  end


  test "connect, subscribe, multiple filters" do
    state = 1
    spec = nil
    human = nil
    spec_ev_uuid = nil
    human_ev_uuid = nil

    authorize_with :active

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe', filters: [['object_uuid', 'is_a', 'arvados#human']]}.to_json)
        ws.send ({method: 'subscribe', filters: [['object_uuid', 'is_a', 'arvados#specimen']]}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          state = 2
        when 2
          assert_equal 200, d["status"]
          spec = Specimen.create
          Trait.create # not part of filters, should not be received
          human = Human.create
          state = 3
        when 3
          spec_ev_uuid = d["object_uuid"]
          state = 4
        when 4
          human_ev_uuid = d["object_uuid"]
          state = 5
          ws.close
        when 5
          assert false, "Should not get any more events"
        end
      end

    end

    assert_not_nil spec
    assert_not_nil human
    assert_equal spec.uuid, spec_ev_uuid
    assert_equal human.uuid, human_ev_uuid
  end


  test "connect, subscribe, compound filter" do
    state = 1
    t1 = nil

    authorize_with :active

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe', filters: [['object_uuid', 'is_a', 'arvados#trait'], ['event_type', '=', 'update']]}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          t1 = Trait.create("name" => "foo")
          t1.name = "bar"
          t1.save!
          state = 2
         when 2
          assert_equal 'update', d['event_type']
          state = 3
          ws.close
        when 3
          assert false, "Should not get any more events"
        end
      end

    end

    assert_equal 3, state
    assert_not_nil t1
  end

  test "connect, subscribe, ask events starting at seq num" do
    state = 1
    human = nil
    human_ev_uuid = nil

    authorize_with :active

    lastid = logs(:admin_changes_specimen).id
    l1 = nil
    l2 = nil

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe', last_log_id: lastid}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          state = 2
        when 2
          l1 = d["object_uuid"]
          assert_not_nil l1, "Unexpected message: #{d}"
          state = 3
        when 3
          l2 = d["object_uuid"]
          assert_not_nil l2, "Unexpected message: #{d}"
          state = 4
          ws.close
        when 4
          assert false, "Should not get any more events"
        end
      end
    end

    expect_next_logs = Log.where('id > ?', lastid).order('id asc')
    assert_equal expect_next_logs[0].object_uuid, l1
    assert_equal expect_next_logs[1].object_uuid, l2
  end

  slow_test "connect, subscribe, get event, unsubscribe" do
    state = 1
    spec = nil
    spec_ev_uuid = nil
    filter_id = nil

    authorize_with :active

    ws_helper(token: :active, timeout: false) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe'}.to_json)
        EM::Timer.new 3 do
          # Set a time limit on the test because after unsubscribing the server
          # still has to process the next event (and then hopefully correctly
          # decides not to send it because we unsubscribed.)
          ws.close
        end
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          spec = Specimen.create
          state = 2
        when 2
          spec_ev_uuid = d["object_uuid"]
          ws.send ({method: 'unsubscribe'}.to_json)

          EM::Timer.new 1 do
            Specimen.create
          end

          state = 3
        when 3
          assert_equal 200, d["status"]
          state = 4
        when 4
          assert false, "Should not get any more events"
        end
      end

    end

    assert_not_nil spec
    assert_equal spec.uuid, spec_ev_uuid
  end

  slow_test "connect, subscribe, get event, unsubscribe with filter" do
    state = 1
    spec = nil
    spec_ev_uuid = nil

    authorize_with :active

    ws_helper(token: :active, timeout: false) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe', filters: [['object_uuid', 'is_a', 'arvados#human']]}.to_json)
        EM::Timer.new 6 do
          # Set a time limit on the test because after unsubscribing the server
          # still has to process the next event (and then hopefully correctly
          # decides not to send it because we unsubscribed.)
          ws.close
        end
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          spec = Human.create
          state = 2
        when 2
          spec_ev_uuid = d["object_uuid"]
          ws.send ({method: 'unsubscribe', filters: [['object_uuid', 'is_a', 'arvados#human']]}.to_json)

          EM::Timer.new 1 do
            Human.create
          end

          state = 3
        when 3
          assert_equal 200, d["status"]
          state = 4
        when 4
          assert false, "Should not get any more events"
        end
      end

    end

    assert_not_nil spec
    assert_equal spec.uuid, spec_ev_uuid
  end


  slow_test "connect, subscribe, get event, try to unsubscribe with bogus filter" do
    state = 1
    spec = nil
    spec_ev_uuid = nil
    human = nil
    human_ev_uuid = nil

    authorize_with :active

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe'}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          spec = Specimen.create
          state = 2
        when 2
          spec_ev_uuid = d["object_uuid"]
          ws.send ({method: 'unsubscribe', filters: [['foo', 'bar', 'baz']]}.to_json)

          EM::Timer.new 1 do
            human = Human.create
          end

          state = 3
        when 3
          assert_equal 404, d["status"]
          state = 4
        when 4
          human_ev_uuid = d["object_uuid"]
          state = 5
          ws.close
        when 5
          assert false, "Should not get any more events"
        end
      end

    end

    assert_not_nil spec
    assert_not_nil human
    assert_equal spec.uuid, spec_ev_uuid
    assert_equal human.uuid, human_ev_uuid
  end

  slow_test "connected, not subscribed, no event" do
    authorize_with :active

    ws_helper(token: :active, timeout: false) do |ws|
      ws.on :open do |event|
        EM::Timer.new 1 do
          Specimen.create
        end

        EM::Timer.new 3 do
          ws.close
        end
      end

      ws.on :message do |event|
        assert false, "Should not get any messages, message was #{event.data}"
      end
    end
  end

  slow_test "connected, not authorized to see event" do
    state = 1

    authorize_with :admin

    ws_helper(token: :active, timeout: false) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'subscribe'}.to_json)

        EM::Timer.new 3 do
          ws.close
        end
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          Specimen.create
          state = 2
        when 2
          assert false, "Should not get any messages, message was #{event.data}"
        end
      end

    end

  end

  test "connect, try bogus method" do
    status = nil

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({method: 'frobnabble'}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        status = d["status"]
        ws.close
      end
    end

    assert_equal 400, status
  end

  test "connect, missing method" do
    status = nil

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send ({fizzbuzz: 'frobnabble'}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        status = d["status"]
        ws.close
      end
    end

    assert_equal 400, status
  end

  test "connect, send malformed request" do
    status = nil

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        ws.send '<XML4EVER></XML4EVER>'
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        status = d["status"]
        ws.close
      end
    end

    assert_equal 400, status
  end


  test "connect, try subscribe too many filters" do
    state = 1

    authorize_with :active

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        (1..17).each do |i|
          ws.send ({method: 'subscribe', filters: [['object_uuid', '=', i]]}.to_json)
        end
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when (1..Rails.configuration.websocket_max_filters)
          assert_equal 200, d["status"]
          state += 1
        when (Rails.configuration.websocket_max_filters+1)
          assert_equal 403, d["status"]
          ws.close
        end
      end

    end

    assert_equal Rails.configuration.websocket_max_filters+1, state

  end

  slow_test "connect, subscribe, lots of events" do
    state = 1
    event_count = 0
    log_start = Log.order(:id).last.id

    authorize_with :active

    ws_helper(token: :active, timeout: false) do |ws|
      EM::Timer.new 45 do
        # Needs a longer timeout than the default
        ws.close
      end

      ws.on :open do |event|
        ws.send ({method: 'subscribe'}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          ActiveRecord::Base.transaction do
            (1..202).each do
              spec = Specimen.create
            end
          end
          state = 2
        when 2
          event_count += 1
          assert_equal d['id'], event_count+log_start
          if event_count == 202
            ws.close
          end
        end
      end

    end

    assert_equal 202, event_count
  end


  test "connect, subscribe with invalid filter" do
    state = 1
    human = nil
    human_ev_uuid = nil

    authorize_with :active

    ws_helper(token: :active) do |ws|
      ws.on :open do |event|
        # test that #6451 is fixed (invalid filter crashes websockets)
        ws.send ({method: 'subscribe', filters: [['object_blarg', 'is_a', 'arvados#human']]}.to_json)
      end

      ws.on :message do |event|
        d = Oj.strict_load event.data
        case state
        when 1
          assert_equal 200, d["status"]
          Specimen.create
          human = Human.create
          state = 2
        when 2
          assert_equal 500, d["status"]
          state = 3
          ws.close
        when 3
          assert false, "Should not get any more events"
        end
      end

    end

    assert_equal 3, state

    # Try connecting again, ensure that websockets server is still running and
    # didn't crash per #6451
    subscribe_test()

  end


end
